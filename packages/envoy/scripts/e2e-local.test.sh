#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly project_root
readonly driver="${project_root}/packages/envoy/scripts/e2e-local.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
readonly output_file="${temporary_dir}/output"

cleanup() {
  rm -f "$output_file"
  rmdir "$temporary_dir"
}
trap cleanup EXIT

bash "$driver" --help >"$output_file"
[[ "$(<"$output_file")" == *"Usage: e2e-local.sh"* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: --help describes the local driver and starts no server\n'

if bash "$driver" --not-an-option >"$output_file" 2>&1; then
  cat "$output_file" >&2
  exit 1
fi
[[ "$(<"$output_file")" == *"unknown option: --not-an-option"* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: rejects unsupported arguments and starts no server\n'
