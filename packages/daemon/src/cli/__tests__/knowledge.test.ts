import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import { mkdir, mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { resolveLegionPaths } from "../../daemon/paths";
import { knowledgeCommand, resolveKnowledgeWorkspaceContext } from "../index";

interface RunnableCommand {
  subCommands?: Record<string, unknown>;
}

describe("knowledge CLI", () => {
  const originalCwd = process.cwd();
  let homeDir = "";

  beforeEach(async () => {
    homeDir = await mkdtemp(path.join(os.tmpdir(), "legion-cli-knowledge-"));
  });

  afterEach(async () => {
    process.chdir(originalCwd);
    if (homeDir) {
      await rm(homeDir, { recursive: true, force: true });
      homeDir = "";
    }
  });

  it("infers legion context from cwd under the workspaces directory", async () => {
    const env = {
      XDG_DATA_HOME: path.join(homeDir, "data"),
      XDG_STATE_HOME: path.join(homeDir, "state"),
    } satisfies Record<string, string>;
    const { workspacesDir } = resolveLegionPaths(env, homeDir);
    const workspaceRoot = path.join(workspacesDir, "trajectory-labs-pbc", "240", "worker-a");
    await mkdir(workspaceRoot, { recursive: true });
    process.chdir(workspaceRoot);

    expect(resolveKnowledgeWorkspaceContext(undefined, env, homeDir)).toEqual({
      legionId: "trajectory-labs-pbc/240",
      workspaceRoot,
    });
  });

  it("registers the consolidate subcommand", () => {
    const subCommands = (knowledgeCommand as RunnableCommand).subCommands;
    expect(subCommands).toBeDefined();
    expect(subCommands).toHaveProperty("consolidate");
  });
});
