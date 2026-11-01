import { afterAll, describe, expect, test } from "bun:test";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// Stage 4b names its two throwaway Postgres containers for the driver's pid (legion-e2e4b-pg-$$,
// legion-e2e4b-db-$$), and the kernel hands a pid out again, so a container of either name can be
// an earlier run's and still be serving it. remove_run_container removes one only while it carries
// this run's own id as the label legion.dev/e2e-run, which the run puts on every container it
// starts. The docker below records each call and answers `inspect` with LABEL, or fails as docker
// does for a name no container has when LABEL is unset.
const script = join(import.meta.dir, "..", "stage4b-sandbox-tree.sh");
const fn = scriptFunctions(script);
const dir = mkdtempSync(join(tmpdir(), "stage4b-run-containers-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
const id = "4242-1759240000000000000";
const container = "legion-e2e4b-db-4242";
// The container id docker answers with, which the removal uses in place of the name.
const cid = "c0ffee1234";
const inspect = `inspect -f {{.Id}} {{index .Config.Labels "legion.dev/e2e-run"}} ${container}`;

let runs = 0;
function removeRun(label: string | null) {
  const calls = join(dir, `calls-${++runs}`);
  const ran = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
run_container_label=legion.dev/e2e-run run_container_id=${id}
docker() {
  printf '%s\\n' "$*" >>${JSON.stringify(calls)}
  case $1 in
    inspect)
      [ -n "\${LABEL+set}" ] || { echo "Error: No such object: $4" >&2; return 1; }
      printf '%s %s\\n' ${cid} "$LABEL" ;;
  esac
}
${fn("remove_run_container")}
remove_run_container ${container}
echo "returned $?"`,
    ],
    {
      env: label === null ? { PATH: "" } : { PATH: "", LABEL: label },
      stdout: "pipe",
      stderr: "pipe",
    }
  );
  return {
    code: ran.exitCode,
    stdout: ran.stdout.toString(),
    calls: existsSync(calls) ? readFileSync(calls, "utf8").split("\n").filter(Boolean) : [],
  };
}

describe("remove_run_container", () => {
  test("removes the container this run started, with its volumes", () => {
    const r = removeRun(id);
    expect(r.code).toBe(0);
    expect(r.stdout).toBe("returned 0\n");
    expect(r.calls).toEqual([inspect, `rm -f -v ${cid}`]);
  });

  test("leaves a container of the same name that an earlier run started, whose pid this run has", () => {
    const r = removeRun("4242-1759100000000000000");
    expect(r.code).toBe(0);
    expect(r.stdout).toBe("returned 0\n");
    expect(r.calls).toEqual([inspect]);
  });

  test("leaves a container of that name carrying no such label at all", () => {
    const r = removeRun("");
    expect(r.code).toBe(0);
    expect(r.calls).toEqual([inspect]);
  });

  test("passes a name no container has, and a docker that cannot answer, without failing the teardown", () => {
    const r = removeRun(null);
    expect(r.code).toBe(0);
    expect(r.stdout).toBe("returned 0\n");
    expect(r.calls).toEqual([inspect]);
  });

  // The teardown removes by label alone, so a container the run starts without one is left on the
  // devbox until a person removes it. Both of the run's containers are started here.
  test("the script labels every container it starts with the run's own id", () => {
    const lines = readFileSync(script, "utf8").split("\n");
    const started = lines.filter(
      (line) => line.startsWith("docker run ") || line.startsWith("session_db_start ")
    );
    expect(started.length).toBe(2);
    for (const line of started) {
      expect(line).toContain('--label "$run_container_label=$run_container_id"');
    }
  });
});
