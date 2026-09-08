#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly project_root
readonly driver="${project_root}/packages/envoy/scripts/e2e-local.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
readonly docker_call_file="${temporary_dir}/docker-calls"
readonly output_file="${temporary_dir}/output"
readonly fake_docker="${temporary_dir}/docker"

cleanup() {
  rm -f "$docker_call_file" "$output_file" "$fake_docker"
  rmdir "$temporary_dir"
}
trap cleanup EXIT

cat >"$fake_docker" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${E2E_TEST_DOCKER_CALL_FILE:?}"
exit 99
EOF
chmod +x "$fake_docker"

PATH="${temporary_dir}:${PATH}" E2E_TEST_DOCKER_CALL_FILE="$docker_call_file" \
  bash "$driver" --help >"$output_file"
[[ "$(<"$output_file")" == *"Usage: e2e-local.sh"* ]] || {
  cat "$output_file" >&2
  exit 1
}
[[ ! -s "$docker_call_file" ]] || {
  cat "$docker_call_file" >&2
  exit 1
}
printf 'PASS: --help describes the local driver without starting Docker\n'

if PATH="${temporary_dir}:${PATH}" E2E_TEST_DOCKER_CALL_FILE="$docker_call_file" \
  bash "$driver" --not-an-option >"$output_file" 2>&1; then
  cat "$output_file" >&2
  exit 1
fi
[[ "$(<"$output_file")" == *"unknown option: --not-an-option"* ]] || {
  cat "$output_file" >&2
  exit 1
}
[[ ! -s "$docker_call_file" ]] || {
  cat "$docker_call_file" >&2
  exit 1
}
printf 'PASS: rejects unsupported arguments without starting Docker\n'
