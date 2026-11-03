import { describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// start_daemon is run after the scratch session database binds its externally reachable port.
// The database's published binding, rather than a free-port check, proves that listener remains
// this run's database; the daemon's two ports must still be free for every boot and restart.
const fn = scriptFunctions(join(import.meta.dir, "..", "stage4b-sandbox-tree.sh"));
const driver = [fn("ports_free"), fn("start_daemon")].join("\n");

function run(sessionDatabaseBinding: string, sessionDatabaseHolder: string) {
  const work = mkdtempSync(join(tmpdir(), "stage4b-daemon-ports-test."));
  writeFileSync(join(work, "legion"), "#!/usr/bin/env bash\nexec sleep 60\n", { mode: 0o755 });
  const shell = `set -Eeuo pipefail
exec 9>&1 7>&1
${driver}
fail() { printf '%s\\n' "$*" >&2; return 1; }
ss() {
  case "$*" in
    *"sport = :$port_session_db"*) printf '%s\\n' "$SESSION_DATABASE_HOLDER" ;;
  esac
}
docker() {
  [ "$1" = port ] && [ "$2" = "$session_db_container" ] && [ "$3" = 5432/tcp ] || return 2
  printf '%s\\n' "$SESSION_DATABASE_BINDING"
}
record_stream() { :; }
until_true() { return 0; }
ports_ours() { :; }
host=10.1.20.250
port_daemon=13373
port_worker_stream=13374
port_session_db=13375
session_db_container=legion-e2e4b-db-test
work=$WORK
daemon_log=$WORK/daemon.log
app_key_unsets=()
start_daemon
kill "$daemon_pid"
wait "$daemon_pid" || true
`;
  try {
    const result = Bun.spawnSync(["bash", "-c", shell], {
      env: {
        ...process.env,
        SESSION_DATABASE_BINDING: sessionDatabaseBinding,
        SESSION_DATABASE_HOLDER: sessionDatabaseHolder,
        WORK: work,
      },
    });
    return { code: result.exitCode, err: result.stderr.toString() };
  } finally {
    rmSync(work, { recursive: true, force: true });
  }
}

describe("start_daemon session database ownership", () => {
  test("boots while this run's session database holds its port", () => {
    const result = run(
      "10.1.20.250:13375",
      'LISTEN 0 4096 10.1.20.250:13375 0.0.0.0:* users:(("docker-proxy",pid=4242,fd=4))'
    );

    expect(result.code, result.err).toBe(0);
    expect(result.err).toBe("");
  });

  test("refuses a foreign holder of the session database port before boot", () => {
    const result = run(
      "10.1.20.250:13376",
      'LISTEN 0 4096 10.1.20.250:13375 0.0.0.0:* users:(("foreign-postgres",pid=4243,fd=4))'
    );

    expect(result.code).toBe(1);
    expect(result.err).toContain("port 13375");
    expect(result.err).toContain("foreign-postgres");
  });
});
