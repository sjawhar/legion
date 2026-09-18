import { expect, test } from "bun:test";
import { launchArgv, workerEnvironment } from "./run";

const launch = {
  rig: "/rig",
  port: 13399,
  omp: "/opt/omp/bin/omp",
  profile: "rig-profile",
};

const inherited = {
  ANTHROPIC_API_KEY: "personal-key-must-not-reach-worker",
  GEMINI_API_KEY: "gemini-key",
  OPENAI_API_KEY: "openai-key",
  LEGION_GRANT: "outer-grant",
  DISPATCH_TOKEN: "outer-dispatch-token",
  PATH: "/outer/worker-bin:/usr/bin",
};

test("strips an inherited Anthropic key from both worker launch modes", () => {
  for (const [mode, useSecrets] of [
    ["rpc", true],
    ["tui", false],
  ] as const) {
    const workerEnv = workerEnvironment({ ...launch, useSecrets }, inherited);

    expect(workerEnv).not.toHaveProperty("ANTHROPIC_API_KEY");
    expect(workerEnv).toMatchObject({
      GEMINI_API_KEY: "gemini-key",
      OPENAI_API_KEY: "openai-key",
      PATH: "/rig/state/worker-bin:/rig/state/bin:/usr/bin",
    });
    expect(launchArgv({ ...launch, useSecrets }, mode)).toEqual(
      useSecrets
        ? ["secrets", "GEMINI_API_KEY", "OPENAI_API_KEY", "--", "/opt/omp/bin/omp", "--mode", "rpc"]
        : ["/opt/omp/bin/omp"]
    );
  }
});
