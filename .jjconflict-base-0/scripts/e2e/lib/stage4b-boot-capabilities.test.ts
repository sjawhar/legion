import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// capabilities_probed, taken from stage4b-sandbox-tree.sh by name, is what boot waits on before it
// reads the daemon's capability report: the probe logs its pass before the daemon records it. Under
// LEGION-663's rendering a live row (subagents, web-search, mcp, repository-extensions,
// dispatch-envoy-tools, github) is `unchecked` until a session reports, which at boot none has, so
// the wait is for the seven image rows alone: a wait for no row unchecked would never return. The
// state is the daemon API fixture (packages/contracts/fixtures/daemon-api/state.json), the wire
// shape the daemon renders: every image row present, every live row unchecked.
const root = join(import.meta.dir, "..", "..", "..");
const fn = scriptFunctions(join(root, "scripts", "e2e", "stage4b-sandbox-tree.sh"));
const fixture = JSON.parse(
  readFileSync(join(root, "packages", "contracts", "fixtures", "daemon-api", "state.json"), "utf8")
) as { capabilities: { name: string; status: string; detail: string }[] };

const imageRows = ["eval-js", "eval-python", "browser", "lsp", "codegraph", "skills", "toolchain"];
const liveRows = [
  "subagents",
  "web-search",
  "mcp",
  "repository-extensions",
  "dispatch-envoy-tools",
  "github",
];

/** Runs capabilities_probed as the script does, with daemon_state answering STATE; 0 is probed. */
function probed(state: unknown): number {
  const ran = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
daemon_state() { printf '%s' "$STATE"; }
${fn("capabilities_probed")}
capabilities_probed`,
    ],
    { env: { ...process.env, STATE: JSON.stringify(state) } }
  );
  if (ran.exitCode !== 0 && ran.exitCode !== 1)
    throw new Error(`capabilities_probed exited ${ran.exitCode}: ${ran.stderr}`);
  return ran.exitCode;
}
const withStatus = (name: string, status: string, detail: string) => ({
  capabilities: fixture.capabilities.map((row) =>
    row.name === name ? { ...row, status, detail } : row
  ),
});

describe("capabilities_probed: boot waits for the seven image rows, not the live rows", () => {
  test("the fixture is the state boot reads: image rows present, live rows unchecked", () => {
    const status: Record<string, string> = Object.fromEntries(
      fixture.capabilities.map((row) => [row.name, row.status])
    );
    for (const name of imageRows) expect(status[name]).toBe("present");
    for (const name of liveRows) expect(status[name]).toBe("unchecked");
  });

  test("probed while every live row is still unchecked", () => {
    expect(probed(fixture)).toBe(0);
  });

  test("not probed while one image row is unchecked", () => {
    expect(probed(withStatus("codegraph", "unchecked", "no probe has reported yet"))).toBe(1);
  });

  test("a live row that reads open or present changes nothing", () => {
    expect(probed(withStatus("web-search", "open", "web_search is not a registered tool"))).toBe(0);
    expect(probed(withStatus("github", "present", "proved by 1 live session"))).toBe(0);
  });
});
