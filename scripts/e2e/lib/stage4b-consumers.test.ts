import { afterAll, describe, expect, test } from "bun:test";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// Stage 4b's delete_consumers against the durable consumers the daemon opens on production NATS
// (packages/daemon/internal/intake/consume.go: one `"legion-go-" + project + "-<suffix>"` name per
// durable). Every run of the script shares the project's consumers with the next: prerequisites
// refuse a run while any consumer under legion-go-<project>- remains, and hygiene (check 54) runs
// delete_consumers and fails on anything left under that prefix, so a durable the daemon opens and
// the function does not delete fails every run at hygiene and refuses the one after it at
// prerequisites. The function is taken from the script by name and run against a nats_stream that
// lists the daemon's consumers under the prefix and records each name it is asked to delete.
const root = join(import.meta.dir, "..", "..", "..");
const fn = scriptFunctions(join(import.meta.dir, "..", "stage4b-sandbox-tree.sh"));
const dir = mkdtempSync(join(tmpdir(), "stage4b-consumers-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

describe("delete_consumers", () => {
  test("deletes every durable consumer the daemon opens for the project", () => {
    // The suffixes consume.go declares, read from its source so a durable added to the daemon
    // reaches this test without anyone naming it here.
    const source = readFileSync(
      join(root, "packages", "daemon", "internal", "intake", "consume.go"),
      "utf8"
    );
    const suffixes = [...source.matchAll(/"legion-go-" \+ project \+ "-([a-z-]+)"/g)].map(
      (m) => m[1]
    );
    expect(suffixes).toContain("dispatch");
    expect(suffixes).toContain("github");
    const project = "LEGSMOKE";
    const present = suffixes.map((suffix) => `legion-go-${project}-${suffix}`);
    const deletedFile = join(dir, "deleted");
    const result = Bun.spawnSync(
      [
        "bash",
        "-c",
        `set -Eeuo pipefail
project=${project} nats_url=nats://nats.example:4222 stream=notifications
consumers_prefix="legion-go-$project-"
note() { :; }
# nats_stream as the script's lib/nats-stream.ts answers: consumers lists the project's durables
# under the prefix, one {"name"} line each; delete prints deleted and records the name.
nats_stream() {
  case "$1" in
    consumers) printf '%s\\n' "$PRESENT" | jq -c -R --arg prefix "$4" 'select(startswith($prefix)) | {name: .}' ;;
    delete) printf '%s\\n' "$4" >>"$DELETED"; echo deleted ;;
    *) echo "unexpected nats_stream op $1" >&2; exit 1 ;;
  esac
}
${fn("delete_consumers")}
delete_consumers
`,
      ],
      { env: { PATH: process.env.PATH ?? "", PRESENT: present.join("\n"), DELETED: deletedFile } }
    );
    expect(result.exitCode).toBe(0);
    const deleted = existsSync(deletedFile)
      ? readFileSync(deletedFile, "utf8")
          .split("\n")
          .filter((line) => line !== "")
      : [];
    for (const name of present) {
      expect(
        deleted,
        `delete_consumers must delete ${name}, a durable the daemon opens; hygiene fails and the next run's prerequisites refuse while it remains`
      ).toContain(name);
    }
  });
});
