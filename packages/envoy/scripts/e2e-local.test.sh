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
# A PATH holding every command the driver needs except a NATS backend: neither docker nor
# nats-server is linked into it, which is the only way to reach the driver's refusal on a host
# that has no container runtime to hide behind.
readonly backendless_path_dir="${temporary_dir}/no-nats-backend"

cleanup() {
  rm -f "$docker_call_file" "$output_file" "$fake_docker"
  rm -rf "$backendless_path_dir"
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

mkdir "$backendless_path_dir"
# bash, dirname, rm and cat are what the driver needs before and during its own exit trap; the
# rest are the commands its required-command loop asks for.
for command in bash bun cat curl dirname go jq openssl rm; do
  command_path="$(command -v "$command")" || {
    printf 'ERR: required command is unavailable: %s\n' "$command" >&2
    exit 3
  }
  ln -s "$command_path" "${backendless_path_dir}/${command}"
done

backendless_status=0
PATH="$backendless_path_dir" E2E_NATS_SERVER="" "$(command -v bash)" "$driver" \
  >"$output_file" 2>&1 || backendless_status=$?
[[ "$backendless_status" == 3 ]] || {
  printf 'ERR: the driver exited %s without a NATS backend, want 3\n' "$backendless_status" >&2
  cat "$output_file" >&2
  exit 1
}
[[ "$(<"$output_file")" == *"required command is unavailable: docker"*"nats-server"* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: refuses to run with neither docker nor nats-server, naming both\n'

# The image lives in one place both sides read. .github/workflows/envoy-and-contracts.yaml pulls
# it ahead of the run by scraping this declaration out of the driver; it used to scrape the
# docker run line instead, and stopped finding it the moment that line moved into a function and
# became indented, so the pull step failed and the acceptance step was skipped. This case runs
# that step's own sed, so inlining the image back into docker run fails here rather than in CI.
scraped_image="$(sed -n 's/^nats_image="\([^"]*\)".*$/\1/p' "$driver")"
[[ -n "$scraped_image" ]] || {
  printf 'ERR: no nats_image declaration at column 0 of %s\n' "$driver" >&2
  exit 1
}
grep -q 'docker run .*"\$nats_image"' "$driver" || {
  printf 'ERR: the docker run line of %s does not start $nats_image (%s)\n' "$driver" "$scraped_image" >&2
  exit 1
}
printf 'PASS: the NATS image is declared once, where the workflow scrapes it: %s\n' "$scraped_image"
