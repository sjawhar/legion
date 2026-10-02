import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const library = fileURLToPath(new URL(".", import.meta.url));
const selector = `include "design-gate-approval-requests"; approval_requested_versions($artifact)`;
const script = readFileSync(
  fileURLToPath(new URL("../stage4b-sandbox-tree.sh", import.meta.url)),
  "utf8"
);

function handedBackVersions(events: unknown[]): string {
  const run = Bun.spawnSync(["jq", "-r", "-L", library, "--arg", "artifact", "spec-1", selector], {
    stdin: new TextEncoder().encode(JSON.stringify(events)),
  });
  if (run.exitCode !== 0) throw new Error(`jq exited ${run.exitCode}: ${run.stderr.toString()}`);
  return run.stdout.toString();
}

const approval = (version: number, requested: number) => ({
  artifact_id: "spec-1",
  name: "spec.md",
  version,
  requested_version: requested,
});
// The previous half of an ask.edited payload as Dispatch writes it (model.AskEditPrevious): no
// approval field.
const previous = {
  question: "Approve spec.md (version 1)? Asks early.",
  options: [],
  multiple: false,
  urgency: "high",
};

describe("drive_gated_spec's hand-back versions", () => {
  test("uses the shared approval-event selector", () => {
    expect(script).toContain('include "design-gate-approval-requests"');
    expect(script).not.toContain("previous.approval");
  });

  test("a request at v1, the move to v2 a block answer caused, and the reworded hand-back at v2 read versions 1 and 2", () => {
    const events = [
      { type: "ask.opened", payload: { id: "a", kind: "approval", approval: approval(1, 1) } },
      {
        type: "ask.edited",
        payload: { id: "a", kind: "approval", approval: approval(2, 1), previous },
      },
      // A hand-back with a new summary rewords the request, then hands it back.
      {
        type: "ask.edited",
        payload: { id: "a", kind: "approval", approval: approval(2, 1), previous },
      },
      { type: "ask.handed_back", payload: { id: "a", kind: "approval", approval: approval(2, 2) } },
      { type: "ask.answered", payload: { id: "a", kind: "approval", approval: approval(2, 2) } },
    ];
    expect(handedBackVersions(events)).toBe("1\n2\n");
  });

  test("a version the request moved to but was never handed back at is not a requested version", () => {
    const events = [
      { type: "ask.opened", payload: { id: "a", kind: "approval", approval: approval(1, 1) } },
      {
        type: "ask.edited",
        payload: { id: "a", kind: "approval", approval: approval(2, 1), previous },
      },
    ];
    expect(handedBackVersions(events)).toBe("1\n");
  });
});
