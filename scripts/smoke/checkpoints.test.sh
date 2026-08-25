#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly checkpoints_script="${project_root}/scripts/smoke/checkpoints.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
readonly fake_bin="${temporary_dir}/bin"
readonly smoke_dir="${temporary_dir}/smoke"
output_file="${temporary_dir}/output"
trap 'rm -rf "$temporary_dir"' EXIT

mkdir -p "$fake_bin" "$smoke_dir"
printf 'none\n' >"${smoke_dir}/webhook-mode"
printf 'none\n' >"${smoke_dir}/board-scope"
cat >"${fake_bin}/curl" <<'EOF'
#!/usr/bin/env bash
printf '%s' '{"issues":{"trajectory-labs-pbc/legion-smoke#1":{}},"trees":{"trajectory-labs-pbc/legion-smoke#1":{"root":"trajectory-labs-pbc/legion-smoke#1"}},"admission":{"active":["trajectory-labs-pbc/legion-smoke#1"]}}'
EOF
cat >"${fake_bin}/tmux" <<'EOF'
#!/usr/bin/env bash
printf 'controller\ntrajectory_hlabs_hpbc__legion_hsmoke-1\n'
EOF
cat >"${fake_bin}/gh" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "${fake_bin}/curl" "${fake_bin}/gh" "${fake_bin}/tmux"

PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="trajectory-labs-pbc/legion-smoke" \
  SMOKE_PROJECT="trajectory-labs-pbc/24" \
  SMOKE_PROJECT_ID="PVT_kwDODfEZEs4BhWFj" \
  bash "$checkpoints_script" 1 >"$output_file" 2>&1
[[ "$(<"$output_file")" == *'CHECKPOINT 1 OK'* ]] || {
  cat "$output_file" >&2
  exit 1
}

PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="trajectory-labs-pbc/legion-smoke" \
  SMOKE_PROJECT="trajectory-labs-pbc/24" \
  SMOKE_PROJECT_ID="PVT_kwDODfEZEs4BhWFj" \
  bash "$checkpoints_script" 2 >"$output_file" 2>&1
[[ "$(<"$output_file")" == *'CHECKPOINT 2 OK'* ]] || {
  cat "$output_file" >&2
  exit 1
}

if PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="trajectory-labs-pbc/legion-smoke" \
  SMOKE_PROJECT="trajectory-labs-pbc/24" \
  bash "$checkpoints_script" 5 >"$output_file" 2>&1; then
  printf 'expected none mode to block the live PR checkpoint\n' >&2
  exit 1
else
  status=$?
fi
[[ "$status" == 3 && "$(<"$output_file")" == *'requires live GitHub webhook ingress'* ]] || {
  cat "$output_file" >&2
  exit 1
}

printf 'envoy\n' >"${smoke_dir}/webhook-mode"
if PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="trajectory-labs-pbc/legion-smoke" \
  SMOKE_PROJECT="trajectory-labs-pbc/24" \
  bash "$checkpoints_script" 5 >"$output_file" 2>&1; then
  printf 'expected checkpoint 5 to require a PR fixture\n' >&2
  exit 1
else
  status=$?
fi
[[ "$status" == 1 && "$(<"$output_file")" == *'set SMOKE_PR or open a legion/issue-* pull request'* ]] || {
  cat "$output_file" >&2
  exit 1
}
printf 'PASS: allows resync checkpoints and blocks live-event checkpoints in none mode\n'
