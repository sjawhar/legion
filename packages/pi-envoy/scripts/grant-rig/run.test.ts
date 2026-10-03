import { afterAll, expect, test } from "bun:test";
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { launchArgv, workerPane } from "./run";

// The rig's state directory: the daemon's pane environment makes the pane's home under it.
const rig = mkdtempSync(path.join(tmpdir(), "grant-rig-"));
const launch = { rig, port: 13399, omp: "/opt/omp/bin/omp", profile: "rig-profile" };

// The pane names the gh, git and jj the daemon resolves on its PATH; this directory holds them.
const tools = mkdtempSync(path.join(tmpdir(), "grant-rig-tools-"));
for (const tool of ["gh", "git", "jj"]) {
  writeFileSync(path.join(tools, tool), "#!/bin/sh\n");
  chmodSync(path.join(tools, tool), 0o755);
}
afterAll(() => {
  rmSync(rig, { recursive: true, force: true });
  rmSync(tools, { recursive: true, force: true });
});

const inherited = {
  ANTHROPIC_API_KEY: "personal-key-must-not-reach-worker",
  GEMINI_API_KEY: "gemini-key",
  OPENAI_API_KEY: "openai-key",
  LEGION_GRANT: "outer-grant",
  DISPATCH_TOKEN: "outer-dispatch-token",
  GH_TOKEN: "outer-github-token",
  PATH: `/outer/worker-bin:${tools}`,
};

// The pane environment comes from the daemon's own functions, compiled and run by `go run`, which
// can take minutes on a cold build cache.
test("strips an inherited Anthropic key and GitHub token from both worker launch modes", () => {
  const workerEnv = workerPane(launch, inherited).env;

  expect(workerEnv).not.toHaveProperty("ANTHROPIC_API_KEY");
  expect(workerEnv).not.toHaveProperty("LEGION_GRANT");
  expect(workerEnv).not.toHaveProperty("DISPATCH_TOKEN");
  expect(workerEnv).not.toHaveProperty("GH_TOKEN");
  expect(workerEnv).toMatchObject({
    GEMINI_API_KEY: "gemini-key",
    OPENAI_API_KEY: "openai-key",
    PATH: `${rig}/state/worker-bin:${rig}/state/bin:/outer/worker-bin:${tools}`,
    LEGION_JJ_PATH: path.join(tools, "jj"),
    LEGION_GRANT_FILE: `${rig}/state/secrets/legion-l12rig-rig-1-implementer-grant`,
    XDG_CONFIG_HOME: `${rig}/state/home/.config`,
  });
  for (const [mode, useSecrets] of [
    ["rpc", true],
    ["tui", false],
  ] as const) {
    // `secrets` reads the caller's secretsd configuration; Oh My Pi gets the pane's XDG_CONFIG_HOME.
    expect(launchArgv({ ...launch, useSecrets }, mode, workerEnv)).toEqual(
      useSecrets
        ? [
            ...[
              "env",
              "-u",
              "XDG_CONFIG_HOME",
              "secrets",
              "GEMINI_API_KEY",
              "OPENAI_API_KEY",
              "--",
            ],
            ...[
              "env",
              `XDG_CONFIG_HOME=${rig}/state/home/.config`,
              "/opt/omp/bin/omp",
              "--mode",
              "rpc",
            ],
          ]
        : ["/opt/omp/bin/omp"]
    );
  }
}, 600_000);
