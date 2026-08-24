#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly project_root
readonly down_script="${project_root}/scripts/smoke/down.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
readonly fake_bin="${temporary_dir}/bin"
readonly smoke_dir="${temporary_dir}/smoke"
bridge_pid=""

cleanup() {
  if [[ -n "$bridge_pid" ]]; then
    kill "$bridge_pid" 2>/dev/null || true
    wait "$bridge_pid" 2>/dev/null || true
  fi
  rm -rf "$temporary_dir"
}
trap cleanup EXIT

mkdir -p "$fake_bin" "$smoke_dir"
cat >"${fake_bin}/docker" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
cat >"${fake_bin}/tmux" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
chmod +x "${fake_bin}/docker" "${fake_bin}/tmux"

sleep 300 &
bridge_pid="$!"
printf '%s\n' "$bridge_pid" >"${smoke_dir}/envoy-bridge.pid"
awk '{print $22}' "/proc/${bridge_pid}/stat" >"${smoke_dir}/envoy-bridge.start"

PATH="${fake_bin}:${PATH}" SMOKE_DIR="$smoke_dir" bash "$down_script" >/dev/null
if kill -0 "$bridge_pid" 2>/dev/null; then
  printf 'expected down.sh to stop the Envoy bridge\n' >&2
  exit 1
fi
bridge_pid=""
printf 'PASS: stops the Envoy bridge with a start-time-validated PID record\n'
