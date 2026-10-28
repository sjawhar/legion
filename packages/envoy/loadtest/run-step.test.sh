#!/usr/bin/env bash
# packages/envoy/loadtest/run-step.test.sh
#
# Proves that one LOADTEST_ROOT runs one step or series at a time, without a real stack: fakes `go`
# and `uv` on PATH. The fake `go` records each call with the name of the run that made it, and the
# first run's call blocks on a FIFO, so the first run stays inside `stack.sh setup` while the test
# starts a second run on the same root.
#
# On a live stack a second run-step.sh on the root of a running step stops that step's Dispatch
# and listener in its reset, and the first step then finishes with exit 0 and a row of connection
# failures: few of its writes commit, few of its requests get 2xx, and it has no CPU or heap
# profile. So the second run must be refused before it touches the stack:
# before `stack.sh setup`, whose first build is the fake `go` call this test watches for.
#
# Asserts that: (a) a second run-step.sh is refused while a run-step.sh holds the root; (b) a
# run-series.sh is refused while a run-step.sh holds it; (c) a run-step.sh is refused while a
# run-series.sh holds it; (d) once the first run has exited, a new run on the root is not refused.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly run_step="${here}/run-step.sh"
readonly run_series="${here}/run-series.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
readonly fake_bin_dir="${temporary_dir}/bin"
readonly calls_file="${temporary_dir}/go-calls"
readonly release_fifo="${temporary_dir}/release"

first_pid=""
cleanup() {
  if [[ -n "$first_pid" ]] && kill -0 "$first_pid" 2>/dev/null; then
    # Unblock the fake go, then stop the first run.
    { printf 'release\n' >"$release_fifo"; } 2>/dev/null &
    kill -TERM "$first_pid" 2>/dev/null || true
    wait "$first_pid" 2>/dev/null || true
  fi
  rm -rf "$temporary_dir"
}
trap cleanup EXIT

mkdir -p "$fake_bin_dir"
cat >"${fake_bin_dir}/go" <<'EOF'
#!/usr/bin/env bash
printf '%s go %s\n' "${LOADTEST_TEST_RUN:?}" "$*" >>"${LOADTEST_TEST_CALLS:?}"
if [[ "$LOADTEST_TEST_RUN" == first ]]; then
  read -r _ <"${LOADTEST_TEST_RELEASE:?}"
fi
exit 1
EOF
cat >"${fake_bin_dir}/uv" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
chmod +x "${fake_bin_dir}/go" "${fake_bin_dir}/uv"

# A checkout that holds what run-step.sh and stack.sh look for before the build.
readonly src="${temporary_dir}/src"
mkdir -p "${src}/packages/envoy/cmd/dispatch" "${src}/packages/envoy/internal/dispatch/store/migrations"
: >"${src}/packages/envoy/internal/dispatch/store/migrations/0001_init.up.sql"

export PATH="${fake_bin_dir}:${PATH}"
export LOADTEST_SRC="$src"
export LOADTEST_TEST_CALLS="$calls_file"
export LOADTEST_TEST_RELEASE="$release_fifo"

step_args=(--events 0 --sessions 1 --requests 0)

# start_first ROOT CMD...: start CMD as the first run on ROOT and wait until it is inside the fake
# go, holding whatever it holds of the root.
start_first() {
  local root=$1
  shift
  : >"$calls_file"
  rm -f "$release_fifo"
  mkfifo "$release_fifo"
  LOADTEST_ROOT="$root" LOADTEST_TEST_RUN=first "$@" >"${temporary_dir}/first-output" 2>&1 &
  first_pid=$!
  local i
  for ((i = 0; i < 300; i++)); do
    grep -q '^first go ' "$calls_file" 2>/dev/null && return 0
    if ! kill -0 "$first_pid" 2>/dev/null; then
      cat "${temporary_dir}/first-output" >&2
      printf 'FAIL: the first run exited before it reached its build\n' >&2
      exit 1
    fi
    sleep 0.1
  done
  cat "${temporary_dir}/first-output" >&2
  printf 'FAIL: the first run did not reach its build within 30 s\n' >&2
  exit 1
}

stop_first() {
  printf 'release\n' >"$release_fifo"
  wait "$first_pid" 2>/dev/null || true
  first_pid=""
}

# expect_refused ROOT WHAT CMD...: CMD, run on ROOT while the first run holds it, exits non-zero
# and never reaches its build.
expect_refused() {
  local root=$1 what=$2
  shift 2
  local rc=0
  LOADTEST_ROOT="$root" LOADTEST_TEST_RUN=second "$@" >"${temporary_dir}/second-output" 2>&1 || rc=$?
  if grep -q '^second go ' "$calls_file"; then
    cat "${temporary_dir}/second-output" >&2
    printf 'FAIL: %s was not refused: it went on to stack.sh setup (its go build ran), so on a live stack it would reset the running step'"'"'s Dispatch and listener\n' "$what" >&2
    exit 1
  fi
  if ((rc == 0)); then
    cat "${temporary_dir}/second-output" >&2
    printf 'FAIL: %s exited 0 while another run held the root\n' "$what" >&2
    exit 1
  fi
}

root="${temporary_dir}/root-a"
start_first "$root" bash "$run_step" first-step "${step_args[@]}"
expect_refused "$root" "a second run-step.sh on the root of a running run-step.sh" \
  bash "$run_step" second-step "${step_args[@]}"
printf 'PASS: a second run-step.sh is refused while a run-step.sh holds LOADTEST_ROOT\n'
expect_refused "$root" "a run-series.sh on the root of a running run-step.sh" bash "$run_series"
printf 'PASS: a run-series.sh is refused while a run-step.sh holds LOADTEST_ROOT\n'
stop_first

root="${temporary_dir}/root-b"
start_first "$root" bash "$run_series"
expect_refused "$root" "a run-step.sh on the root of a running run-series.sh" \
  bash "$run_step" second-step "${step_args[@]}"
printf 'PASS: a run-step.sh is refused while a run-series.sh holds LOADTEST_ROOT\n'
stop_first

# Once the first run has exited, the root is free again.
: >"$calls_file"
LOADTEST_ROOT="$root" LOADTEST_TEST_RUN=third bash "$run_step" third-step "${step_args[@]}" \
  >"${temporary_dir}/third-output" 2>&1 || true
if ! grep -q '^third go ' "$calls_file"; then
  cat "${temporary_dir}/third-output" >&2
  printf 'FAIL: a run-step.sh after the first run exited did not reach its build: the root stayed held\n' >&2
  exit 1
fi
printf 'PASS: a run on LOADTEST_ROOT after the first run exited is not refused\n'
