import { describe, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../../..", import.meta.url));
const script = readFileSync(
  fileURLToPath(new URL("../stage4b-sandbox-tree.sh", import.meta.url)),
  "utf8"
);

// drive_gated_spec's own requested-versions command, from its `requested_versions=$(jq` line to the
// line that feeds it the events.
const requestedVersionsCommand = (() => {
  const lines = script.split("\n");
  const start = lines.findIndex((text) => /^\s*requested_versions=\$\(jq /.test(text));
  const end = lines.findIndex((text, index) => index > start && /<<<"\$events"\)\s*$/.test(text));
  if (start < 0 || end < 0) {
    throw new Error(
      "drive_gated_spec's requested_versions command is not in stage4b-sandbox-tree.sh"
    );
  }
  return lines.slice(start, end + 1).join("\n");
})();

// The versions drive_gated_spec reads as handed back, by its own command run through bash from a
// directory that is not lib/, as the driver runs it.
function handedBackVersions(events: unknown[]): string {
  const run = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -euo pipefail\n${requestedVersionsCommand}\nprintf '%s\\n' "$requested_versions"`,
    ],
    {
      cwd: tmpdir(),
      env: { ...process.env, root, artifact: "spec-1", events: JSON.stringify(events) },
    }
  );
  if (run.exitCode !== 0) throw new Error(`bash exited ${run.exitCode}: ${run.stderr.toString()}`);
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

// drive_gated_spec's own verdict line, run as the driver runs it: through bash, from a directory
// that is not lib/, on files shaped as Dispatch serves them.
describe("drive_gated_spec's verdict", () => {
  test("the driver's verdict command compiles and judges a hand-back at the approved version", () => {
    const line = script
      .split("\n")
      .find((text) => /^\s*verdict=\$\(jq .*design-gate-verdict\.jq/.test(text));
    expect(line).toBeDefined();
    const evidence = mkdtempSync(join(tmpdir(), "design-gate-driver-"));
    try {
      const question = "Approve spec.md (version 2)? Adds the budget.";
      const ask = {
        id: "a",
        kind: "approval",
        question,
        approval: approval(2, 2),
        state: "answered",
      };
      writeFileSync(join(evidence, "T-1-asks.json"), JSON.stringify([ask]));
      writeFileSync(
        join(evidence, "T-1-events.json"),
        JSON.stringify([
          {
            type: "ask.opened",
            created_at: "2026-10-02T10:00:00Z",
            payload: {
              ...ask,
              question: "Approve spec.md (version 1)? Asks.",
              approval: approval(1, 1),
            },
          },
          { type: "ask.handed_back", created_at: "2026-10-02T10:05:00Z", payload: ask },
          { type: "ask.answered", created_at: "2026-10-02T10:06:00Z", payload: ask },
        ])
      );
      for (const number of [1, 2]) {
        writeFileSync(
          join(evidence, `T-1-spec-v${number}.json`),
          JSON.stringify({ number, markdown: "The plan." })
        );
      }
      const run = Bun.spawnSync(
        [
          "bash",
          "-c",
          `set -euo pipefail; requested=("$evidence/T-1-spec-v1.json" "$evidence/T-1-spec-v2.json")\n${line}\nprintf '%s\\n' "$verdict"`,
        ],
        {
          cwd: evidence,
          env: {
            ...process.env,
            root,
            artifact: "spec-1",
            approved: "2",
            evidence,
            issue: "T-1",
          },
        }
      );
      expect(run.stderr.toString()).toBe("");
      expect(run.exitCode).toBe(0);
      expect(JSON.parse(run.stdout.toString())).toEqual({
        request: question,
        summarized: true,
        blocks: 0,
        early: [],
      });
    } finally {
      rmSync(evidence, { recursive: true, force: true });
    }
  });
});
