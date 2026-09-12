#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly project_root
readonly down_script="${project_root}/scripts/smoke/down.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
readonly fake_bin="${temporary_dir}/bin"
readonly smoke_dir="${temporary_dir}/smoke"
readonly output_file="${temporary_dir}/output"
bridge_pid=""
forward_pgid=""

cleanup() {
  if [[ -n "$bridge_pid" ]]; then
    kill "$bridge_pid" 2>/dev/null || true
    wait "$bridge_pid" 2>/dev/null || true
  fi
  if [[ -n "$forward_pgid" ]]; then
    kill -- "-$forward_pgid" 2>/dev/null || true
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

tmux_log="${temporary_dir}/tmux.log"
cat >"${fake_bin}/tmux" <<EOF
#!/usr/bin/env bash
if [[ "\$1" == "-L" ]]; then shift 2; fi
case "\$1" in
  has-session) exit 0 ;;
  show-option) printf 'foreign-owner\n' ;;
  kill-session) printf 'kill-session\n' >>"${tmux_log}" ;;
esac
EOF
chmod +x "${fake_bin}/tmux"

PATH="${fake_bin}:${PATH}" SMOKE_DIR="$smoke_dir" SMOKE_PROJECT="omp" bash "$down_script" >"$output_file" 2>&1
[[ ! -e "$tmux_log" && "$(<"$output_file")" == *'refusing to kill unowned tmux session'* ]] || {
  cat "$output_file" >&2
  exit 1
}

cat >"${fake_bin}/tmux" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"${tmux_log}"
if [[ "\$1" == "-L" ]]; then shift 2; fi
case "\$1" in
  has-session) exit 0 ;;
  show-option) printf 'legion-omp\n' ;;
esac
EOF
chmod +x "${fake_bin}/tmux"

PATH="${fake_bin}:${PATH}" SMOKE_DIR="$smoke_dir" SMOKE_PROJECT="omp" bash "$down_script" >"$output_file" 2>&1
# The kill must land on the daemon's private socket -- the default server never hosts a Legion
# session, so a bare `tmux kill-session` would find nothing (or worse, an operator's own session).
[[ "$(<"$tmux_log")" == *'-L legion-omp kill-session -t legion-omp'* ]] || {
  cat "$output_file" >&2
  cat "$tmux_log" >&2
  exit 1
}
[[ "$(<"$output_file")" == *'STOPPED tmux session legion-omp on private socket legion-omp'* ]] || {
  cat "$output_file" >&2
  exit 1
}

# forward mode teardown: a recorded `gh webhook forward` process group and its GitHub hook
# record. `gh webhook forward` itself needs a user-authenticated gh identity agents do not have,
# so this stand-in is the only proof the rig has that forward-mode teardown works. The stand-in
# reports its own pid from inside the new session: reading /proc/$!/stat right after `setsid &`
# races the setsid() call and could record this harness's own process group, which down.sh would
# then kill -- so the harness also refuses to continue unless that pid is its own group leader.
forward_pid_file="${temporary_dir}/forward.pid"
setsid bash -c 'printf "%s\n" "$$" >"$1"; exec sleep 300' _ "$forward_pid_file" &
for ((attempt = 1; attempt <= 100; attempt += 1)); do
  [[ -s "$forward_pid_file" ]] && break
  sleep 0.05
done
[[ -s "$forward_pid_file" ]] || {
  printf 'fixture error: the forwarder stand-in never reported its pid\n' >&2
  exit 1
}
forward_pgid="$(<"$forward_pid_file")"
[[ "$(awk '{print $5}' "/proc/${forward_pgid}/stat")" == "$forward_pgid" ]] || {
  printf 'fixture error: forwarder stand-in %s is not its own process-group leader\n' "$forward_pgid" >&2
  forward_pgid=""
  exit 1
}
printf '%s\n' "$forward_pgid" >"${smoke_dir}/webhook-forward.pid"
awk '{print $22}' "/proc/${forward_pgid}/stat" >"${smoke_dir}/webhook-forward.start"
printf 'repos/example-org/legion-smoke/hooks/7\n' >"${smoke_dir}/webhook-forward.hook"
gh_log="${temporary_dir}/gh.log"
cat >"${fake_bin}/gh" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"${gh_log}"
EOF
chmod +x "${fake_bin}/gh"

PATH="${fake_bin}:${PATH}" SMOKE_DIR="$smoke_dir" SMOKE_REPO="example-org/legion-smoke" bash "$down_script" >"$output_file" 2>&1
if kill -0 -- "-$forward_pgid" 2>/dev/null; then
  printf 'expected down.sh to kill the recorded webhook-forward process group\n' >&2
  exit 1
fi
forward_pgid=""
[[ ! -e "${smoke_dir}/webhook-forward.pid" && ! -e "${smoke_dir}/webhook-forward.start" && ! -e "${smoke_dir}/webhook-forward.hook" ]] || {
  printf 'expected down.sh to remove the webhook-forward pid, start, and hook records\n' >&2
  exit 1
}
grep -Fxq 'api -X DELETE repos/example-org/legion-smoke/hooks/7' "$gh_log" || {
  printf 'expected down.sh to delete the recorded forwarder hook through gh api; gh log:\n%s\n' "$(cat "$gh_log" 2>/dev/null)" >&2
  exit 1
}
[[ "$(<"$output_file")" == *'RIG DOWN'* ]] || {
  cat "$output_file" >&2
  exit 1
}

printf 'PASS: only kills tmux sessions carrying the Legion ownership marker\n'
printf 'PASS: stops the Envoy bridge with a start-time-validated PID record\n'
printf 'PASS: forward-mode teardown kills the recorded forwarder process group and deletes its hook record\n'
