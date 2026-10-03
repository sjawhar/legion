import { afterAll, expect, test } from "bun:test";
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { launchArgv, workerEnvironment } from "./run";

const launch = {
  rig: "/rig",
  port: 13399,
  omp: "/opt/omp/bin/omp",
  profile: "rig-profile",
};

// The pane names the gh, git and jj the daemon resolves on its PATH; this directory holds them.
const tools = mkdtempSync(path.join(tmpdir(), "grant-rig-tools-"));
for (const tool of ["gh", "git", "jj"]) {
  writeFileSync(path.join(tools, tool), "#!/bin/sh\n");
  chmodSync(path.join(tools, tool), 0o755);
}
afterAll(() => rmSync(tools, { recursive: true, force: true }));

const inherited = {
  ANTHROPIC_API_KEY: "personal-key-must-not-reach-worker",
  GEMINI_API_KEY: "gemini-key",
  OPENAI_API_KEY: "openai-key",
  LEGION_GRANT: "outer-grant",
  DISPATCH_TOKEN: "outer-dispatch-token",
  GH_TOKEN: "outer-github-token",
  PATH: `/outer/worker-bin:${tools}`,
};

test("strips an inherited Anthropic key and GitHub token from both worker launch modes", () => {
  for (const [mode, useSecrets] of [
    ["rpc", true],
    ["tui", false],
  ] as const) {
    const worker = { ...launch, useSecrets };
    const workerEnv = workerEnvironment(worker, inherited);

    expect(workerEnv).not.toHaveProperty("ANTHROPIC_API_KEY");
    expect(workerEnv).not.toHaveProperty("LEGION_GRANT");
    expect(workerEnv).not.toHaveProperty("GH_TOKEN");
    expect(workerEnv).toMatchObject({
      GEMINI_API_KEY: "gemini-key",
      OPENAI_API_KEY: "openai-key",
      PATH: `/rig/state/worker-bin:/rig/state/bin:${tools}`,
      LEGION_JJ_PATH: path.join(tools, "jj"),
      LEGION_GRANT_FILE: "/rig/state/secrets/legion-l12rig-rig-1-implementer-grant",
    });
    expect(launchArgv(worker, mode)).toEqual(
      useSecrets
        ? ["secrets", "GEMINI_API_KEY", "OPENAI_API_KEY", "--", "/opt/omp/bin/omp", "--mode", "rpc"]
        : ["/opt/omp/bin/omp"]
    );
  }
});
