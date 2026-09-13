#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly project_root
readonly up_script="${project_root}/scripts/smoke/up.sh"
source_dir="$(mktemp -d)"
warning_file="$(mktemp)"
assertion_file="$(mktemp)"
fake_bin="$(mktemp -d)"
headers_file="$(mktemp)"
body_file="$(mktemp)"
response_file="$(mktemp)"
gh_call_file="$(mktemp)"
order_log="$(mktemp)"
actor_body_file="$(mktemp)"
main_output_file="$(mktemp)"
trap 'rm -f "$warning_file" "$assertion_file" "$headers_file" "$body_file" "$response_file" "$gh_call_file" "$order_log" "$actor_body_file" "$main_output_file"; rm -rf "$fake_bin" "$source_dir"' EXIT
export SMOKE_DIR="${fake_bin}/smoke"

# up.sh sources dispatch-config.sh from its own directory (by BASH_SOURCE), so the stripped copy
# needs that file beside it; the copy is what the harness sources, exactly as before. The copy
# resolves repo_root from its own mktemp location, so it is pinned to this harness's project
# root: main() must build and launch from the checkout whatever the cwd.
sed -e '$d' -e "s|^repo_root=.*|repo_root=\"${project_root}\"|" "$up_script" >"${source_dir}/up.sh"
cp "${project_root}/scripts/smoke/dispatch-config.sh" "${source_dir}/dispatch-config.sh"
grep -Fxq "repo_root=\"${project_root}\"" "${source_dir}/up.sh" || {
  printf 'fixture error: repo_root was not pinned in the sourced copy\n' >&2
  exit 1
}
# shellcheck source=/dev/null
source "${source_dir}/up.sh"

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
  printf 'expected repository owner for the default installation-token owner\n' >&2
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
  *)
    printf 'unexpected gh invocation: %q\n' "$*" >&2
    exit 1
    ;;
esac
EOF
chmod +x "${fake_bin}/gh"


export SMOKE_PROJECT="sjawhar/24"
mkdir -p "$SMOKE_DIR"
write_daemon_config
[[ "$(<"${SMOKE_DIR}/legion.yaml")" == *$'repos:\n  - sjawhar/legion-smoke'* ]] || {
  printf 'expected generated daemon config to pin repos to SMOKE_REPO\n' >&2
  exit 1
}
[[ "$(<"${SMOKE_DIR}/legion.yaml")" == *$'\n'"instructions: ${SMOKE_DIR}/deployment-instructions.md"$'\n'* ]] || {
  printf 'expected generated daemon config to point instructions at the rig file\n' >&2
  exit 1
}
[[ "$(<"${SMOKE_DIR}/deployment-instructions.md")" == *"repository sjawhar/legion-smoke"* ]] || {
  printf 'expected the rig deployment-instructions file to name SMOKE_REPO\n' >&2
  exit 1
}


# The default-mode cases must not see an inherited SMOKE_WEBHOOK_MODE: the README's start block
# exports it in an operator's shell, so each clears it in its own subshell, exactly as the
# LEGION-40 cases below clear DISPATCH_URL/DISPATCH_TOKEN.
[[ "$(unset SMOKE_WEBHOOK_MODE; SMOKE_GH_WEBHOOK_HELP_EXIT=0 resolve_webhook_mode)" == "forward" ]] || {
  printf 'expected available webhook forwarding to default to forward mode\n' >&2
  exit 1
}
[[ "$(unset SMOKE_WEBHOOK_MODE; SMOKE_GH_WEBHOOK_HELP_EXIT=1 resolve_webhook_mode)" == "none" ]] || {
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
: >"$gh_call_file"
[[ "$(SMOKE_WEBHOOK_MODE=envoy SMOKE_GH_CALL_FILE="$gh_call_file" resolve_webhook_mode)" == "envoy" ]] || {
  printf 'expected SMOKE_WEBHOOK_MODE=envoy to select the production Envoy bridge\n' >&2
  exit 1
}
[[ ! -s "$gh_call_file" ]] || {
  printf 'SMOKE_WEBHOOK_MODE=envoy must not invoke gh\n' >&2
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
[[ "$(webhook_ingress_block_reason)" == "SMOKE_WEBHOOK_MODE=none: no Dispatch issue event reaches the rig NATS, so the daemon never admits the root issue (resync skips issue keys it never ingested), and no live GitHub event does either; checkpoints 1-4 and 12 need SMOKE_WEBHOOK_MODE=envoy, 5-7 and 9-11 need envoy or forward; checkpoints 8 and 13 are not gated by the mode" ]] || {
  printf 'expected the none-mode block reason to name the missing Dispatch issue-event feed and envoy for checkpoints 1-4 and 12\n' >&2
  exit 1
}

printf 'PASS: selects webhook ingress mode without silently falling back\n'

fake_omp="${fake_bin}/omp"
printf '#!/usr/bin/env bash\nexit 0\n' >"$fake_omp"
chmod +x "$fake_omp"
[[ "$(LEGION_OMP_PATH="$fake_omp" resolve_omp_path)" == "$fake_omp" ]] || {
  printf 'expected an explicit executable LEGION_OMP_PATH to pass through unchanged\n' >&2
  exit 1
}
if (LEGION_OMP_PATH="${fake_bin}/missing-omp" resolve_omp_path) >"$assertion_file" 2>&1; then
  printf 'expected a missing LEGION_OMP_PATH to fail\n' >&2
  exit 1
fi
[[ "$(<"$assertion_file")" == *"LEGION_OMP_PATH is not an absolute executable file: ${fake_bin}/missing-omp"* ]] || {
  cat "$assertion_file" >&2
  exit 1
}
# Each guard must fail for its own reason, so each case satisfies every other guard:
# an existing, executable file reached by a relative path (only the leading-slash test can
# reject it), and an existing absolute file with no execute bit (only -x can).
if (cd "$fake_bin" && LEGION_OMP_PATH="omp" resolve_omp_path) >"$assertion_file" 2>&1; then
  printf 'expected a relative LEGION_OMP_PATH to fail even when the file exists and is executable\n' >&2
  exit 1
fi
[[ "$(<"$assertion_file")" == *'LEGION_OMP_PATH is not an absolute executable file: omp'* ]] || {
  cat "$assertion_file" >&2
  exit 1
}
fake_omp_noexec="${fake_bin}/omp-noexec"
printf '#!/usr/bin/env bash\nexit 0\n' >"$fake_omp_noexec"
chmod -x "$fake_omp_noexec"
if (LEGION_OMP_PATH="$fake_omp_noexec" resolve_omp_path) >"$assertion_file" 2>&1; then
  printf 'expected a non-executable LEGION_OMP_PATH to fail\n' >&2
  exit 1
fi
[[ "$(<"$assertion_file")" == *"LEGION_OMP_PATH is not an absolute executable file: ${fake_omp_noexec}"* ]] || {
  cat "$assertion_file" >&2
  exit 1
}
# Unset LEGION_OMP_PATH: the daemon resolves the pin with `mise where` and never installs, so
# preflight runs the same lookup. A fake mise on the harness PATH stands in for the operator's
# mise: `where` prints an install directory (whose bin/omp preflight then checks), or fails.
fake_mise_install="${fake_bin}/mise-install"
mkdir -p "${fake_mise_install}/bin"
printf '#!/usr/bin/env bash\nexit 0\n' >"${fake_mise_install}/bin/omp"
chmod +x "${fake_mise_install}/bin/omp"
cat >"${fake_bin}/mise" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"\${SMOKE_MISE_CALL_LOG:-/dev/null}"
[[ "\$1" == "where" ]] || { printf 'fake mise: unexpected subcommand %s\n' "\$1" >&2; exit 2; }
if [[ -n "\${SMOKE_MISE_WHERE_FAILS:-}" ]]; then
  printf 'mise ERROR %s not installed\n' "\$2" >&2
  exit 1
fi
if [[ -n "\${SMOKE_MISE_CONFIG_ERROR:-}" ]]; then
  printf 'mise ERROR Invalid TOML in config file: /etc/mise/config.toml\n' >&2
  exit 1
fi
printf '%s\n' "${fake_mise_install}"
EOF
chmod +x "${fake_bin}/mise"
mise_call_log="${fake_bin}/mise-call.log"
# A valid override is the whole answer: mise is never consulted for it.
: >"$mise_call_log"
[[ "$(LEGION_OMP_PATH="$fake_omp" SMOKE_MISE_CALL_LOG="$mise_call_log" resolve_omp_path 2>&1)" == "$fake_omp" ]] || {
  printf 'expected a valid LEGION_OMP_PATH to resolve without consulting mise\n' >&2
  exit 1
}
[[ ! -s "$mise_call_log" ]] || {
  printf 'a valid LEGION_OMP_PATH must not invoke mise; calls:\n%s\n' "$(<"$mise_call_log")" >&2
  exit 1
}
: >"$mise_call_log"
[[ "$(unset LEGION_OMP_PATH; SMOKE_MISE_CALL_LOG="$mise_call_log" resolve_omp_path)" == "${fake_mise_install}/bin/omp" ]] || {
  printf 'expected an unset LEGION_OMP_PATH to resolve to bin/omp under the directory mise where prints\n' >&2
  exit 1
}
# The pin asked of mise is the one up.sh read from omp-pin.ts, not a literal of this harness.
[[ -n "$omp_pin" && "$(<"$mise_call_log")" == "where ${omp_pin}" ]] || {
  printf 'expected mise where to be asked for the omp-pin.ts pin (%s); calls:\n%s\n' "$omp_pin" "$(<"$mise_call_log")" >&2
  exit 1
}
# The install directory exists but its bin/omp is not executable: preflight still refuses.
chmod -x "${fake_mise_install}/bin/omp"
if (unset LEGION_OMP_PATH; resolve_omp_path) >"$assertion_file" 2>&1; then
  printf 'expected preflight to fail when the installed pin has no executable bin/omp\n' >&2
  exit 1
fi
[[ "$(<"$assertion_file")" == *"${fake_mise_install}/bin/omp is missing or not executable"* && "$(<"$assertion_file")" == *"run: mise install ${omp_pin}"* ]] || {
  cat "$assertion_file" >&2
  exit 1
}
chmod +x "${fake_mise_install}/bin/omp"
# `mise where` fails (pin not installed): mise's own line reaches the output, and preflight stops
# naming the exact mise install command.
if (unset LEGION_OMP_PATH; SMOKE_MISE_WHERE_FAILS=1 resolve_omp_path) >"$assertion_file" 2>&1; then
  printf 'expected preflight to fail when the pin is not installed\n' >&2
  exit 1
fi
[[ "$(<"$assertion_file")" == *"mise ERROR ${omp_pin} not installed"* && "$(<"$assertion_file")" == *"OMP pin ${omp_pin} is not installed"* && "$(<"$assertion_file")" == *"run: mise install ${omp_pin}"* ]] || {
  cat "$assertion_file" >&2
  exit 1
}

printf 'PASS: an explicit LEGION_OMP_PATH override passes through; otherwise the omp-pin.ts pin must be installed under mise, and preflight names mise install when it is not\n'

# `mise where` fails for another reason (a broken mise configuration): preflight shows mise's own
# message and stops without the mise install remedy, which would not fix it.
if (unset LEGION_OMP_PATH; SMOKE_MISE_CONFIG_ERROR=1 resolve_omp_path) >"$assertion_file" 2>&1; then
  printf 'expected preflight to fail when mise where fails on a configuration error\n' >&2
  exit 1
fi
[[ "$(<"$assertion_file")" == *'Invalid TOML in config file'* && "$(<"$assertion_file")" == *"mise where ${omp_pin} failed"* && "$(<"$assertion_file")" != *'mise install'* ]] || {
  printf 'expected mise'"'"'s own message and no mise install remedy; output:\n%s\n' "$(<"$assertion_file")" >&2
  exit 1
}

printf 'PASS: a mise failure other than not-installed stops preflight on mise'"'"'s own message, without the mise install remedy\n'

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

mkdir -p "$SMOKE_DIR"
SMOKE_PROJECT="acme/1" SMOKE_OMP_LAUNCH_PREFIX="" write_daemon_config
[[ "$(grep -c '^omp_launch_prefix: \[\]$' "${SMOKE_DIR}/legion.yaml")" == "1" ]] || {
  printf 'expected omp_launch_prefix: [] when SMOKE_OMP_LAUNCH_PREFIX is explicitly empty, got:\n%s\n' "$(<"${SMOKE_DIR}/legion.yaml")" >&2
  exit 1
}
if grep -q '^  - $' "${SMOKE_DIR}/legion.yaml"; then
  printf 'unexpected blank list item in generated config\n' >&2
  exit 1
fi

printf 'PASS: SMOKE_OMP_LAUNCH_PREFIX="" disables the launch prefix (omp_launch_prefix: [])\n'

SMOKE_PROJECT="acme/1" write_daemon_config
[[ "$(grep -A1 '^omp_launch_prefix:$' "${SMOKE_DIR}/legion.yaml" | tail -1)" == "  - secrets" ]] || {
  printf 'expected the default secrets prefix when SMOKE_OMP_LAUNCH_PREFIX is unset, got:\n%s\n' "$(<"${SMOKE_DIR}/legion.yaml")" >&2
  exit 1
}

printf 'PASS: unset SMOKE_OMP_LAUNCH_PREFIX defaults to the secrets wrapper prefix\n'

# Acceptance 1 (LEGION-10): the real daemon loader accepts the file write_daemon_config emits.
# `--check-config` never resolves secrets (config.ts `resolveSecrets: false`) and the generated
# file sets no dispatch_url -- but resolveDaemonConfig also reads DISPATCH_URL from the
# environment and then demands DISPATCH_TOKEN, so scrub both (and the retired alias) to keep the
# case independent of whatever shell runs this harness.
readonly daemon_cli="${project_root}/packages/daemon/src/cli/index.ts"
SMOKE_PROJECT="acme/1" write_daemon_config
if ! env -u DISPATCH_URL -u DISPATCH_TOKEN -u DISPATCH_MCP_URL \
  bun run "$daemon_cli" start acme/1 --config "${SMOKE_DIR}/legion.yaml" --check-config >"$assertion_file" 2>&1; then
  printf 'expected the daemon loader to accept the generated config; output:\n%s\n' "$(<"$assertion_file")" >&2
  exit 1
fi
[[ "$(<"$assertion_file")" == *'Config OK'* ]] || {
  printf 'expected "Config OK" from --check-config; output:\n%s\n' "$(<"$assertion_file")" >&2
  exit 1
}
printf 'PASS: the real daemon loader accepts the generated legion.yaml (--check-config prints Config OK)\n'

# The retired `merge: human` gates block is exactly what the loader must refuse.
# Anchored on the `gates:` line itself, not on the design gate's value, which the rig sets
# independently.
sed 's/^gates:$/gates:\n  merge: human/' "${SMOKE_DIR}/legion.yaml" >"${SMOKE_DIR}/legion-gates-merge.yaml"
grep -Fxq '  merge: human' "${SMOKE_DIR}/legion-gates-merge.yaml" || {
  printf 'fixture error: the gates.merge line was not inserted\n' >&2
  exit 1
}
if env -u DISPATCH_URL -u DISPATCH_TOKEN -u DISPATCH_MCP_URL \
  bun run "$daemon_cli" start acme/1 --config "${SMOKE_DIR}/legion-gates-merge.yaml" --check-config >"$assertion_file" 2>"$warning_file"; then
  printf 'expected loader to reject gates.merge\n' >&2
  exit 1
fi
[[ "$(<"$warning_file")" == *'gates.merge is not a Legion setting'* ]] || {
  printf 'expected "gates.merge is not a Legion setting" on stderr; stderr:\n%s\n' "$(<"$warning_file")" >&2
  exit 1
}
printf 'PASS: the real daemon loader rejects the retired gates.merge block\n'

# LEGION-40: DISPATCH_URL/DISPATCH_TOKEN resolve from the environment, else from the same envoy.json
# the production launcher reads. XDG_CONFIG_HOME points at harness-owned directories so every case
# is independent of the box's own ~/.config/opencode/envoy.json (present here, absent in CI) and of
# the DISPATCH_URL a Legion pane carries; each subshell also clears both variables explicitly.
xdg_file_dir="${fake_bin}/xdg-with-file"
xdg_scratch_dir="${fake_bin}/xdg-scratch"
mkdir -p "${xdg_file_dir}/opencode" "${xdg_scratch_dir}/opencode"
printf '{"dispatch":{"enabled":true,"serverUrl":"http://file.test","token":"file-token"}}\n' >"${xdg_file_dir}/opencode/envoy.json"

[[ "$(unset DISPATCH_URL DISPATCH_TOKEN; XDG_CONFIG_HOME="$xdg_file_dir" resolve_dispatch_config && printf '%s %s' "$DISPATCH_URL" "$DISPATCH_TOKEN")" == "http://file.test file-token" ]] || {
  printf 'expected DISPATCH_URL and DISPATCH_TOKEN to resolve from envoy.json when both are unset\n' >&2
  exit 1
}
# Assigned, not exported: no child of the resolving shell sees the bearer unless handed it.
[[ "$(unset DISPATCH_URL DISPATCH_TOKEN; XDG_CONFIG_HOME="$xdg_file_dir" resolve_dispatch_config && env | grep -c '^DISPATCH_TOKEN=' || true)" == "0" ]] || {
  printf 'expected resolve_dispatch_config to assign DISPATCH_TOKEN without exporting it\n' >&2
  exit 1
}
printf 'PASS: DISPATCH_URL and DISPATCH_TOKEN fall back to .dispatch.serverUrl/.dispatch.token in envoy.json\n'

# An operator's export reaches the resolver as a child's environment, so these two cases run it in
# a fresh bash whose environment carries the pair exactly as given (a prefix assignment on the child,
# never a write to this shell's own variables) and print the pair it left.
resolved_from_environment() {
  DISPATCH_URL="$1" DISPATCH_TOKEN="$2" XDG_CONFIG_HOME="$xdg_file_dir" \
    bash -c 'fail() { printf "error: %s\n" "$*" >&2; exit 1; }; source "$1"; resolve_dispatch_config && printf "%s %s" "$DISPATCH_URL" "$DISPATCH_TOKEN"' \
    _ "${source_dir}/dispatch-config.sh"
}
[[ "$(resolved_from_environment "http://env.test" "env-token")" == "http://env.test env-token" ]] || {
  printf 'expected an exported DISPATCH_URL/DISPATCH_TOKEN to win over envoy.json\n' >&2
  exit 1
}
# An exported empty string is "not provided", not a value.
[[ "$(resolved_from_environment "" "")" == "http://file.test file-token" ]] || {
  printf 'expected an empty DISPATCH_URL/DISPATCH_TOKEN to fall back to envoy.json\n' >&2
  exit 1
}
printf 'PASS: an exported DISPATCH_URL/DISPATCH_TOKEN wins over envoy.json\n'

# Neither source: the message names the variable, the file, and the key -- the exact text the README
# quotes -- and nothing else is consulted. DISPATCH_URL is checked first, so with both unset and no
# file it is the one named.
if (unset DISPATCH_URL DISPATCH_TOKEN; XDG_CONFIG_HOME="$xdg_scratch_dir" resolve_dispatch_config) >"$assertion_file" 2>&1; then
  printf 'expected resolve_dispatch_config to fail with no environment value and no envoy.json\n' >&2
  exit 1
fi
[[ "$(<"$assertion_file")" == "error: DISPATCH_URL is unset and ${xdg_scratch_dir}/opencode/envoy.json does not exist; export DISPATCH_URL or set .dispatch.serverUrl in that file" ]] || {
  cat "$assertion_file" >&2
  exit 1
}
# The file exists and has the URL but not the token: the production launcher's bare `jq -r` would
# print the literal `null` here; the rig must stop and name .dispatch.token instead.
printf '{"dispatch":{"enabled":true,"serverUrl":"http://file.test"}}\n' >"${xdg_scratch_dir}/opencode/envoy.json"
if (unset DISPATCH_URL DISPATCH_TOKEN; XDG_CONFIG_HOME="$xdg_scratch_dir" resolve_dispatch_config) >"$assertion_file" 2>&1; then
  printf 'expected resolve_dispatch_config to fail when envoy.json lacks .dispatch.token\n' >&2
  exit 1
fi
[[ "$(<"$assertion_file")" == "error: DISPATCH_TOKEN is unset and ${xdg_scratch_dir}/opencode/envoy.json has no .dispatch.token; export DISPATCH_TOKEN or set .dispatch.token in that file" ]] || {
  cat "$assertion_file" >&2
  exit 1
}
# Not JSON at all: reported as such (with jq's own line above it), never as a missing key.
printf '{' >"${xdg_scratch_dir}/opencode/envoy.json"
if (unset DISPATCH_URL DISPATCH_TOKEN; XDG_CONFIG_HOME="$xdg_scratch_dir" resolve_dispatch_config) >"$assertion_file" 2>&1; then
  printf 'expected resolve_dispatch_config to fail on an unparsable envoy.json\n' >&2
  exit 1
fi
[[ "$(<"$assertion_file")" == *"error: DISPATCH_URL is unset and ${xdg_scratch_dir}/opencode/envoy.json could not be parsed as JSON (see the jq error above); export DISPATCH_URL or set .dispatch.serverUrl in that file" ]] || {
  cat "$assertion_file" >&2
  exit 1
}
printf 'PASS: with neither source present, resolution stops naming the variable, the envoy.json path, and the key\n'

export DISPATCH_URL="http://dispatch.test"
export DISPATCH_TOKEN="test-dispatch-token"
printf '{"key":"LEGSMOKE-7"}' >"$response_file"
if ! (ensure_root_issue) >"$assertion_file" 2>&1; then
  cat "$assertion_file" >&2
  exit 1
fi
[[ "$(<"${SMOKE_DIR}/root-issue")" == "LEGSMOKE-7" ]] || {
  printf 'expected ensure_root_issue to record the created Dispatch root issue key\n' >&2
  exit 1
}
[[ "$(<"$assertion_file")" == *'CREATED root issue LEGSMOKE-7'* ]] || {
  printf 'expected a CREATED root issue message\n' >&2
  exit 1
}

# Idempotent rerun against the same SMOKE_DIR: reuses the recorded file instead of creating a
# second Dispatch issue for the same exercise.
printf '{"key":"LEGSMOKE-8"}' >"$response_file"
if ! (ensure_root_issue) >"$assertion_file" 2>&1; then
  cat "$assertion_file" >&2
  exit 1
fi
[[ "$(<"${SMOKE_DIR}/root-issue")" == "LEGSMOKE-7" ]] || {
  printf 'expected ensure_root_issue to reuse the already-recorded root issue on rerun\n' >&2
  exit 1
}
[[ "$(<"$assertion_file")" == *'REUSED root issue LEGSMOKE-7'* ]] || {
  printf 'expected a REUSED root issue message on rerun\n' >&2
  exit 1
}

printf 'PASS: records a created Dispatch root issue and reuses it on a later rerun\n'

export SMOKE_ORDER_LOG="$order_log"
export SMOKE_ACTOR_BODY_FILE="$actor_body_file"

cat >"${fake_bin}/go" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "${fake_bin}/go"

# Overwrites the shared fake curl (unused by any later test -- this is the file's last section)
# so the root-issue POST's actual `-d` body is captured for the actor assertion below.
cat >"${fake_bin}/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
body=""
url=""
while (($#)); do
  case "$1" in
    -d | --data | --data-binary)
      body="$2"
      shift 2
      ;;
    -H | --header)
      shift 2
      ;;
    *)
      url="$1"
      shift
      ;;
  esac
done
case "$url" in
  */api/v1/issues)
    printf 'curl:issues-create\n' >>"$SMOKE_ORDER_LOG"
    printf '%s' "$body" >"$SMOKE_ACTOR_BODY_FILE"
    printf '{"key":"LEGSMOKE-42"}'
    ;;
  *)
    printf 'unexpected curl request: %s\n' "$*" >&2
    exit 1
    ;;
esac
EOF
chmod +x "${fake_bin}/curl"

# Stubs every other main() dependency so the boot sequence runs with no real daemon, tmux pane,
# Docker container, or Go build -- only relative call order and the root-issue POST body matter
# here (`ensure_root_issue` itself, and its underlying curl invocation, are left real).
require_command() { :; }
assert_port_free() { :; }
ensure_nats() { printf 'ensure_nats\n' >>"$order_log"; }
start_process() {
  printf 'start_process:%s\n' "$1" >>"$order_log"
  printf '%s\n' "${@:2}" >"${SMOKE_DIR}/start_process.${1}.argv"
  : >"${SMOKE_DIR}/${1}.log"
}
wait_for_json() { printf 'wait_for_json:%s\n' "$1" >>"$order_log"; }
assert_webhook_round_trip() { printf 'assert_webhook_round_trip\n' >>"$order_log"; }
wait_for_envoy_bridge() { printf 'wait_for_envoy_bridge\n' >>"$order_log"; }

rm -f "${SMOKE_DIR}/root-issue"
export SMOKE_REPO="sjawhar/legion-smoke"
export SMOKE_PROJECT="sjawhar/24"
export GITHUB_WEBHOOK_SECRET="legion-smoke-secret"
export GH_AGENT_APP_PRIVATE_KEY_B64="dummy"
export GH_REVIEW_APP_PRIVATE_KEY_B64="dummy"
export DISPATCH_URL="http://dispatch.test"
export DISPATCH_TOKEN="test-dispatch-token"
export SMOKE_WEBHOOK_MODE="envoy"

# Preflight fails closed before anything starts: a missing LEGION_OMP_PATH stops main() before
# ensure_nats or any start_process call is reached (the stubs above log every such call).
# resolve_omp_path fails inside `omp_path="$(...)"`, which only aborts main() under `set -e`, and
# bash suppresses -e for anything run as an `if` condition -- so the probe runs as a plain
# subshell that re-enables -e itself and reports its status through the substitution.
: >"$order_log"
preflight_status="$(set +e; (set -e; LEGION_OMP_PATH="${fake_bin}/missing-omp" main) >"$assertion_file" 2>&1; echo $?)"
[[ "$preflight_status" != 0 ]] || {
  printf 'expected main() to fail on a missing LEGION_OMP_PATH\n' >&2
  exit 1
}
[[ "$(<"$assertion_file")" == *"LEGION_OMP_PATH is not an absolute executable file: ${fake_bin}/missing-omp"* ]] || {
  cat "$assertion_file" >&2
  exit 1
}
# `GREEN OMP build:` prints only after resolve_omp_path returned successfully. Its absence is what
# proves the failed resolution stopped main() -- a `local omp_path="$(...)"` form would mask the
# failure from set -e, print the line with an empty path, and only stop later for some other reason.
[[ "$(<"$assertion_file")" != *'GREEN OMP build:'* ]] || {
  printf 'expected main() to stop before printing the selected OMP build; output:\n%s\n' "$(<"$assertion_file")" >&2
  exit 1
}
[[ ! -s "$order_log" ]] || {
  printf 'expected no process to start when OMP preflight fails; order log:\n%s\n' "$(<"$order_log")" >&2
  exit 1
}
printf 'PASS: a missing OMP build stops up.sh in preflight before any process starts\n'

# No override: preflight verifies the omp-pin.ts pin through the fake mise on PATH, prints the
# install's bin/omp, and exports nothing -- the daemon resolves the same pin itself.
: >"$order_log"
rm -f "${SMOKE_DIR}"/start_process.*.argv "${SMOKE_DIR}/root-issue"
main_status="$(set +e; (set -e; unset LEGION_OMP_PATH; main) >"$main_output_file" 2>&1; echo $?)"
[[ "$main_status" == 0 ]] || {
  printf 'expected up.sh main() to succeed on the installed pin (exit %s); output:\n%s\n' "$main_status" "$(<"$main_output_file")" >&2
  exit 1
}
[[ "$(<"$main_output_file")" == *'RIG READY'* && "$(<"$main_output_file")" == *"GREEN OMP build: ${fake_mise_install}/bin/omp (mise where ${omp_pin})"* ]] || {
  printf 'expected RIG READY and the pin install bin/omp as the selected OMP build, labelled mise where; output:\n%s\n' "$(<"$main_output_file")" >&2
  exit 1
}
# The glob below must match the argv files main() recorded in envoy mode, or the grep proves
# nothing: with no file it fails on the literal pattern and the check would pass vacuously.
for name in listener daemon envoy-bridge; do
  [[ -f "${SMOKE_DIR}/start_process.${name}.argv" ]] || {
    printf 'fixture error: main() recorded no start_process argv for %s\n' "$name" >&2
    exit 1
  }
done
if grep -l '^LEGION_OMP_PATH=' "${SMOKE_DIR}"/start_process.*.argv; then
  printf 'expected no start_process env block to carry LEGION_OMP_PATH when the operator set none\n' >&2
  exit 1
fi
printf 'PASS: with no override, preflight verifies the omp-pin.ts pin under mise and exports no LEGION_OMP_PATH to the daemon\n'

: >"$order_log"
rm -f "${SMOKE_DIR}"/start_process.*.argv "${SMOKE_DIR}/root-issue"
export LEGION_OMP_PATH="$fake_omp"

main_status="$(set +e; (set -e; main) >"$main_output_file" 2>&1; echo $?)"
[[ "$main_status" == 0 ]] || {
  printf 'expected up.sh main() to succeed (exit %s); output:\n%s\n' "$main_status" "$(<"$main_output_file")" >&2
  exit 1
}

[[ "$(<"$main_output_file")" == *'RIG READY'* ]] || {
  printf 'expected up.sh main() to finish with RIG READY; output:\n%s\n' "$(<"$main_output_file")" >&2
  exit 1
}
[[ "$(<"$main_output_file")" == *"GREEN OMP build: ${fake_omp} (LEGION_OMP_PATH override)"* ]] || {
  printf 'expected up.sh to print the selected OMP build, labelled as the override; output:\n%s\n' "$(<"$main_output_file")" >&2
  exit 1
}
grep -Fxq "LEGION_OMP_PATH=${fake_omp}" "${SMOKE_DIR}/start_process.daemon.argv" || {
  printf 'expected the daemon start_process env block to carry LEGION_OMP_PATH; argv:\n%s\n' "$(<"${SMOKE_DIR}/start_process.daemon.argv")" >&2
  exit 1
}
# Every start_process call recorded its argv (listener, daemon, envoy-bridge): exactly one file
# may carry LEGION_OMP_PATH, and it is the daemon's.
[[ "$(grep -l '^LEGION_OMP_PATH=' "${SMOKE_DIR}"/start_process.*.argv)" == "${SMOKE_DIR}/start_process.daemon.argv" ]] || {
  printf 'up.sh names LEGION_OMP_PATH explicitly in the daemon env block only; argv files carrying it:\n%s\n' "$(grep -l '^LEGION_OMP_PATH=' "${SMOKE_DIR}"/start_process.*.argv || true)" >&2
  exit 1
}
grep -Fxq "omp_invocation: mise x ${omp_pin} -- omp" "${SMOKE_DIR}/legion.yaml" || {
  printf 'expected legion.yaml to keep the mise x <pin> -- omp invocation\n' >&2
  exit 1
}
printf 'PASS: an explicit LEGION_OMP_PATH override is named in the daemon env block and in no other start_process argv, and omp_invocation keeps the mise x <pin> -- omp form\n'

# The Dispatch-ingress record lands beside the webhook-mode and design-gate records: `shared` by
# default, `rig` when the operator says a scratch Dispatch publishes into the rig NATS, and
# anything else refused before the rig starts, naming the two values.
[[ "$(<"${SMOKE_DIR}/dispatch-ingress")" == "shared" && "$(<"${SMOKE_DIR}/webhook-mode")" == "envoy" && "$(<"${SMOKE_DIR}/design-gate")" == "off" ]] || {
  printf 'expected main() to record dispatch-ingress shared beside webhook-mode envoy and design-gate off; got %s / %s / %s\n' "$(<"${SMOKE_DIR}/dispatch-ingress")" "$(<"${SMOKE_DIR}/webhook-mode")" "$(<"${SMOKE_DIR}/design-gate")" >&2
  exit 1
}
[[ "$(SMOKE_DISPATCH_INGRESS=rig resolve_dispatch_ingress)" == "rig" ]] || {
  printf 'expected SMOKE_DISPATCH_INGRESS=rig to be accepted\n' >&2
  exit 1
}
if (SMOKE_DISPATCH_INGRESS=direct resolve_dispatch_ingress) >"$assertion_file" 2>&1; then
  printf 'expected an unknown SMOKE_DISPATCH_INGRESS to be refused\n' >&2
  exit 1
fi
[[ "$(<"$assertion_file")" == *'SMOKE_DISPATCH_INGRESS must be shared or rig'* ]] || {
  cat "$assertion_file" >&2
  exit 1
}
: >"$order_log"
rm -f "${SMOKE_DIR}"/start_process.*.argv "${SMOKE_DIR}/root-issue"
main_status="$(set +e; (set -e; SMOKE_DISPATCH_INGRESS=rig main) >"$main_output_file" 2>&1; echo $?)"
[[ "$main_status" == 0 ]] || {
  printf 'expected up.sh main() to succeed with SMOKE_DISPATCH_INGRESS=rig (exit %s); output:\n%s\n' "$main_status" "$(<"$main_output_file")" >&2
  exit 1
}
[[ "$(<"${SMOKE_DIR}/dispatch-ingress")" == "rig" ]] || {
  printf 'expected main() to record dispatch-ingress rig; got %s\n' "$(<"${SMOKE_DIR}/dispatch-ingress")" >&2
  exit 1
}
printf 'PASS: records the Dispatch ingress (shared by default, rig on request) beside the other rig records and refuses any other value naming both\n'

daemon_ready_line="$(grep -n '^wait_for_json:Legion daemon$' "$order_log" | head -1 | cut -d: -f1)"
bridge_ready_line="$(grep -n '^wait_for_envoy_bridge$' "$order_log" | head -1 | cut -d: -f1)"
root_issue_line="$(grep -n '^curl:issues-create$' "$order_log" | head -1 | cut -d: -f1)"

[[ -n "$daemon_ready_line" && -n "$bridge_ready_line" && -n "$root_issue_line" ]] || {
  printf 'expected daemon-ready, bridge-ready, and root-issue-create markers in the call order log:\n%s\n' "$(<"$order_log")" >&2
  exit 1
}
((daemon_ready_line < root_issue_line)) || {
  printf 'expected the daemon-ready wait before the root-issue POST; call order log:\n%s\n' "$(<"$order_log")" >&2
  exit 1
}
((bridge_ready_line < root_issue_line)) || {
  printf 'expected the envoy-bridge-ready wait before the root-issue POST; call order log:\n%s\n' "$(<"$order_log")" >&2
  exit 1
}

printf 'PASS: creates the Dispatch root issue only after the daemon and envoy bridge report ready\n'

jq -e '.actor.kind == "session" and (.actor.id | type == "string" and length > 0)' >/dev/null "$actor_body_file" || {
  printf 'expected the root-issue creation request to carry actor.kind == "session"; body:\n%s\n' "$(<"$actor_body_file")" >&2
  exit 1
}
jq -e '.force == true' >/dev/null "$actor_body_file" || {
  printf 'expected the root-issue creation body to carry force: true (Dispatch near-duplicate bypass); body:\n%s\n' "$(<"$actor_body_file")" >&2
  exit 1
}
printf 'PASS: root-issue creation request forces past the Dispatch near-duplicate check\n'

printf 'PASS: root-issue creation request carries a session actor for the bearer-authenticated POST\n'
