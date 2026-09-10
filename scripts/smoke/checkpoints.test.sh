#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly checkpoints_script="${project_root}/scripts/smoke/checkpoints.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
readonly fake_bin="${temporary_dir}/bin"
readonly smoke_dir="${temporary_dir}/smoke"
readonly curl_log="${temporary_dir}/curl.log"
export CURL_LOG="$curl_log"
output_file="${temporary_dir}/output"
trap 'rm -rf "$temporary_dir"' EXIT

mkdir -p "$fake_bin" "$smoke_dir"
printf 'none\n' >"${smoke_dir}/webhook-mode"
cat >"${fake_bin}/curl" <<'EOF'
#!/usr/bin/env bash
request="$*"
printf '%s\n' "$request" >>"$CURL_LOG"
case "$request" in
  *"/legion/v1/state"*)
    printf '%s' '{"issues":{"LEGSMOKE-1":{"status":"in_progress","children":["LEGSMOKE-2"]},"LEGSMOKE-2":{"parent":"LEGSMOKE-1","status":"todo","children":[]}},"trees":{"LEGSMOKE-1":{"root":"LEGSMOKE-1","status":"active","locator":{"tmuxWindowId":"@2","tmuxPaneId":"%2"}},"LEGSMOKE-2":{"root":"LEGSMOKE-2","status":"active","locator":{"tmuxWindowId":"@3","tmuxPaneId":"%3"}}},"controllerLocator":{"tmuxWindowId":"@1","tmuxPaneId":"%1"},"admission":{"active":["LEGSMOKE-1","LEGSMOKE-2"]},"gates":{"LEGSMOKE-1":{"designAskId":"ask-design"}}}'
    ;;
  *"/api/v1/issues/LEGSMOKE-1/asks?state=open"*)
    printf '%s' '[{"id":"ask-design","state":"open","options":[{"label":"Approve"}]}]'
    ;;
  *"/api/v1/issues/LEGSMOKE-1/artifacts"*)
    printf '%s' '[{"name":"spec.md","primary":true,"versions":[{"number":1}]}]'
    ;;
  *"/api/v1/issues?project=LEGSMOKE&parent=LEGSMOKE-1"*)
    printf '%s' '[{"key":"LEGSMOKE-2","parent":"LEGSMOKE-1","status":"todo"}]'
    ;;
  *"/api/v1/issues/LEGSMOKE-1"*)
    printf '%s' '{"key":"LEGSMOKE-1","status":"in_progress"}'
    ;;
  *)
    printf 'unexpected curl request: %s\n' "$request" >&2
    exit 1
    ;;
esac
EOF
cat >"${fake_bin}/tmux" <<'EOF'
#!/usr/bin/env bash
if [[ "$*" == *"#{window_id}"* ]]; then
  printf '@1\n@2\n@3\n'
else
  printf 'controller\nlegsmoke-1\nlegsmoke-2\n'
fi
EOF
cat >"${fake_bin}/gh" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "${fake_bin}/curl" "${fake_bin}/gh" "${fake_bin}/tmux"

PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  DISPATCH_TOKEN="test-dispatch-token" \
  bash "$checkpoints_script" 1 >"$output_file" 2>&1
[[ "$(<"$output_file")" == *'CHECKPOINT 1 OK'* ]] || {
  cat "$output_file" >&2
  exit 1
}
grep -Fq 'Authorization: Bearer test-dispatch-token' "$curl_log"
grep -Fq 'http://dispatch.test/api/v1/issues/LEGSMOKE-1' "$curl_log"

PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  DISPATCH_TOKEN="test-dispatch-token" \
  bash "$checkpoints_script" 2 >"$output_file" 2>&1
[[ "$(<"$output_file")" == *'CHECKPOINT 2 OK'* ]] || {
  cat "$output_file" >&2
  exit 1
}
grep -Fq 'http://dispatch.test/api/v1/issues/LEGSMOKE-1' "$curl_log"

PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  DISPATCH_TOKEN="test-dispatch-token" \
  bash "$checkpoints_script" 3 >"$output_file" 2>&1
[[ "$(<"$output_file")" == *'CHECKPOINT 3 OK'* ]] || {
  cat "$output_file" >&2
  exit 1
}
grep -Fq 'http://dispatch.test/api/v1/issues/LEGSMOKE-1/asks?state=open' "$curl_log"
grep -Fq 'http://dispatch.test/api/v1/issues/LEGSMOKE-1/artifacts' "$curl_log"
grep -Fq 'http://dispatch.test/api/v1/issues?project=LEGSMOKE&parent=LEGSMOKE-1' "$curl_log"

PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
  DISPATCH_URL="http://dispatch.test" \
  DISPATCH_TOKEN="test-dispatch-token" \
  bash "$checkpoints_script" 4 >"$output_file" 2>&1
[[ "$(<"$output_file")" == *'CHECKPOINT 4 OK'* ]] || {
  cat "$output_file" >&2
  exit 1
}

if PATH="${fake_bin}:${PATH}" \
  SMOKE_DIR="$smoke_dir" \
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
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
  SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" \
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
