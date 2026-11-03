# shellcheck shell=bash disable=SC2154 # work, session_db, check, evidence are the caller's
# teardown_sequence runs every step that must happen once, in order, whether the run passed or
# failed partway: bridge_stop (the reverse tunnel's pod/Service, by exact name), teardown (the
# issue pods, PVCs, Secrets and ConfigMaps this run's own Sandboxes own, by label), the scratch
# session database container, the agent-secrets re-minter if one is running, and finally
# namespace_clean's own before/after comparison. Called from both the main flow's own passing path
# and the EXIT trap's fallback, so bridge_stop still runs before namespace_clean's snapshot even on
# a passing run (the trap alone fires after the main flow's own calls). A single done flag set
# before the first real step would reintroduce the same ordering mistake in the other direction: an
# interrupt during bridge_stop itself (a signal killing the whole process mid-call, which never
# returns) would leave that flag already true, so a retry from the trap would skip every remaining
# step — teardown, the scratch database, the reminter, namespace_clean — entirely. Each step
# instead has its own done flag, set only once that step has actually succeeded, and every step
# still runs even when an earlier one failed outright (returned nonzero rather than being killed):
# best-effort, not first-failure-stops-the-rest, since each step's cleanup is independent of the
# others succeeding. A later call (the retry a real interrupt needs) skips only the steps already
# marked done and resumes at the first one that is not — exactly where an interrupt lands, or
# nothing more if every step already succeeded. Returns nonzero if any step failed, so the caller
# still learns something went wrong even though every step still ran. Extracted into its own
# sourced file (teardown.test.ts tests it directly, the same way namespace-rig.test.ts tests
# namespace-rig.sh), rather than living inline in the top-level script, which cannot be sourced
# without its own side effects running.
bridge_stop_done=
teardown_done=
session_db_torn_down=
reminter_torn_down=
namespace_clean_done=
teardown_sequence() {
  local rc=0
  if [ -z "$bridge_stop_done" ]; then
    if bridge_stop; then bridge_stop_done=1; else rc=1; fi
  fi
  if [ -z "$teardown_done" ]; then
    if teardown; then teardown_done=1; else rc=1; fi
  fi
  if [ -z "$session_db_torn_down" ]; then
    [ -z "$session_db_started" ] || docker rm -f -v "$session_db" >/dev/null 2>&1 || echo "could not remove the scratch session database container $session_db"
    session_db_torn_down=1
  fi
  if [ -z "$reminter_torn_down" ]; then
    [ -z "$agent_secrets_reminter" ] || kill "$agent_secrets_reminter" 2>/dev/null || true
    reminter_torn_down=1
  fi
  if [ -z "$compared" ] && [ -n "$snapshotted" ] && [ -z "$namespace_clean_done" ]; then
    if namespace_clean; then namespace_clean_done=1; else rc=1; fi
  fi
  return "$rc"
}

# kill_harness stops the Go test process directly, not just the shell's own `wait` on it: cleanup
# calls it defensively, and the watchdog (lib/bridge-tunnel.sh) calls it from both of its own
# give-up paths, since merely interrupting `wait` (which a trapped signal does immediately — the
# harness invocation in stage4a-sandbox-runtime.sh explains why that matters) leaves the harness
# itself running as an orphan unless something also kills the process.
kill_harness() {
  [ -f "$work/harness.pid" ] || return 0
  kill "$(cat "$work/harness.pid")" 2>/dev/null || true
}

cleanup() {
  local status=$?
  # A second signal must not cut the teardown short, and a closed output must not end it.
  trap '' HUP INT TERM PIPE
  exec >&7 2>&7
  set +e
  kill_harness
  (teardown_sequence) || status=1
  rm -rf "$work"
  [ -n "$ok" ] || echo "stage 4a e2e: FAIL (check $check)"
  echo "evidence: $evidence (transcript.log, runtime.log, the namespace snapshots)"
  exit "$status"
}
