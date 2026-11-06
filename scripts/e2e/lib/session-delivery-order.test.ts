import { expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { expectNoMissingHelpers, nameMissingHelpers, scriptFunctions } from "./script-functions";

// envoy-session-delivery.sh's last assertion reads the followed issue's comments out of
// events.jsonl and checks both transcripts show them in the order the stream stored them. Its
// selector and the rows record() writes are two halves of one contract: a selector naming a field
// record() does not write selects nothing, jq fails every row, and the run prints "issue order:
// every comment ... appears in its stream order" having compared nothing, then "ENVOY SESSION
// DELIVERY: PASS (every event once, the issue in order)" having checked no order at all. This row
// runs both halves against each other: the script's own record() writes the rows and the script's
// own selector reads them back.

const script = join(import.meta.dir, "..", "envoy-session-delivery.sh");
const fn = scriptFunctions(script);

/**
 * The jq program envoy-session-delivery.sh selects the followed issue's comment markers with,
 * read from the line that fills `written_order`, so this row runs what the script runs rather
 * than a copy of it.
 */
function orderSelector(): string {
  const found = /^written_order=\$\(jq -r '([\s\S]*?)' "\$events"/m.exec(
    readFileSync(script, "utf8")
  );
  if (found === null) {
    throw new Error(
      "envoy-session-delivery.sh fills written_order in a shape this row cannot read"
    );
  }
  return found[1];
}

test("the issue-order selector reads the comment rows record() writes", () => {
  const dir = mkdtempSync(join(tmpdir(), "session-delivery-order."));
  try {
    const events = join(dir, "events.jsonl");
    // Two comments of one window, exactly as the script records them: the followed issue's
    // comment events, each meant for both sessions and expected once.
    const written = Bun.spawnSync([
      "bash",
      "-c",
      `${nameMissingHelpers(script)}
set -u
events=${JSON.stringify(events)}
: >"$events"
${fn("record")}
marker=LDT0001-001Z
record a issue comment-1 "pi cc" 1 '{"id":"one"}'
marker=LDT0001-002Z
record a issue comment-2 "pi cc" 1 '{"id":"two"}'`,
    ]);
    expectNoMissingHelpers(written.stdout.toString(), written.stderr.toString());
    expect(written.stderr.toString()).toBe("");
    expect(written.exitCode).toBe(0);

    const selected = Bun.spawnSync(["jq", "-r", orderSelector(), events]);
    // jq reports a row it cannot read on stderr and prints nothing for it, so a selector naming
    // an absent field leaves the output empty and the exit status 0.
    expect(selected.stderr.toString()).toBe("");
    expect(selected.stdout.toString()).toBe("LDT0001-001Z\nLDT0001-002Z\n");
  } finally {
    rmSync(dir, { force: true, recursive: true });
  }
});
