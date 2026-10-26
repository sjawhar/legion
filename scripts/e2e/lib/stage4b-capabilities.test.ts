import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// web_search_open_for, taken from stage4b-sandbox-tree.sh by name, is full-agent-negative's read of
// the web-search row once tree 3's planner reports web_search off. It runs here on the row the
// daemon's own renderer writes for that session (packages/daemon/internal/capabilities,
// TestWebSearchOpenRowGolden): the planner of LEGSMOKE-3 in its issue pod legion-omp-legsmoke-3,
// whose roles all run in the one `roles` container.
const root = join(import.meta.dir, "..", "..", "..");
const fn = scriptFunctions(join(root, "scripts", "e2e", "stage4b-sandbox-tree.sh"));
const row = readFileSync(
  join(
    root,
    "packages",
    "daemon",
    "internal",
    "capabilities",
    "testdata",
    "web-search-open-planner.json"
  ),
  "utf8"
);
const pod = "legion-omp-legsmoke-3";
const issue = "LEGSMOKE-3";

function holds(detailRow: string, forPod = pod, forIssue = issue): boolean {
  const result = Bun.spawnSync([
    "bash",
    "-c",
    `set -Eeuo pipefail
${fn("web_search_open_for")}
web_search_open_for "$1" "$2" "$3"`,
    "web-search-open-for",
    detailRow,
    forPod,
    forIssue,
  ]);
  return result.exitCode === 0;
}
const edited = (edit: (detail: string) => string) => {
  const parsed = JSON.parse(row) as { detail: string };
  return JSON.stringify({ ...parsed, detail: edit(parsed.detail) });
};

describe("web_search_open_for on the row the daemon renders", () => {
  test("holds for the planner of the issue in its pod's roles container", () => {
    expect(holds(row)).toBe(true);
  });

  test("refuses another pod", () => {
    expect(holds(row, "legion-omp-legsmoke-4")).toBe(false);
  });

  test("refuses another issue", () => {
    expect(holds(row, pod, "LEGSMOKE-4")).toBe(false);
  });

  test("refuses another role", () => {
    expect(holds(edited((d) => d.replace("(planner, ", "(tester, ")))).toBe(false);
  });

  test("refuses a container per role, the layout before the one roles container", () => {
    expect(holds(edited((d) => d.replace(`${pod}/roles `, `${pod}/planner `)))).toBe(false);
  });

  test("refuses another fact", () => {
    expect(
      holds(edited((d) => d.replace("is not a registered tool", "is not among the active tools")))
    ).toBe(false);
  });

  test("refuses a row that is not open", () => {
    const parsed = JSON.parse(row);
    expect(holds(JSON.stringify({ ...parsed, status: "present" }))).toBe(false);
  });
});
