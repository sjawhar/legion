import { describe, expect, test } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  attachCommand,
  getDaemonPort,
  loadTeamsCache,
  startCommand,
  statusCommand,
  stopCommand,
  teamsCommand,
} from "../index";

async function resolveArgs(command: { args?: unknown }): Promise<Record<string, unknown>> {
  const { args } = command;
  if (!args) {
    throw new Error("Command has no args");
  }
  if (typeof args === "function") {
    return (await args()) as Record<string, unknown>;
  }
  return args as Record<string, unknown>;
}

describe("citty command definitions", () => {
  test("start command args are defined", async () => {
    const args = await resolveArgs(startCommand);
    const team = args.team as { type: string; required?: boolean };
    const workspace = args.workspace as { type: string; alias?: string; default?: string };
    const stateDir = args["state-dir"] as { type: string };
    expect(team.type).toBe("positional");
    expect(team.required).toBe(true);
    expect(workspace.type).toBe("string");
    expect(workspace.alias).toBe("w");
    expect(workspace.default).toBe(process.cwd());
    expect(stateDir.type).toBe("string");
  });

  test("stop command args are defined", async () => {
    const args = await resolveArgs(stopCommand);
    const team = args.team as { type: string; required?: boolean };
    const stateDir = args["state-dir"] as { type: string };
    expect(team.type).toBe("positional");
    expect(team.required).toBe(true);
    expect(stateDir.type).toBe("string");
  });

  test("status command args are defined", async () => {
    const args = await resolveArgs(statusCommand);
    const team = args.team as { type: string; required?: boolean };
    const stateDir = args["state-dir"] as { type: string };
    expect(team.type).toBe("positional");
    expect(team.required).toBe(true);
    expect(stateDir.type).toBe("string");
  });

  test("attach command args are defined", async () => {
    const args = await resolveArgs(attachCommand);
    const team = args.team as { type: string; required?: boolean };
    const issue = args.issue as { type: string; required?: boolean };
    expect(team.type).toBe("positional");
    expect(team.required).toBe(true);
    expect(issue.type).toBe("positional");
    expect(issue.required).toBe(true);
  });

  test("teams command args are defined", async () => {
    const args = await resolveArgs(teamsCommand);
    const all = args.all as { type: string; default?: boolean };
    expect(all.type).toBe("boolean");
    expect(all.default).toBe(false);
  });
});

describe("getDaemonPort", () => {
  test("returns default port when env unset", () => {
    expect(getDaemonPort({} as NodeJS.ProcessEnv)).toBe(13370);
  });

  test("returns env port when valid", () => {
    expect(
      getDaemonPort({ LEGION_DAEMON_PORT: "14400" } as NodeJS.ProcessEnv)
    ).toBe(14400);
  });

  test("falls back when env port invalid", () => {
    expect(
      getDaemonPort({ LEGION_DAEMON_PORT: "not-a-number" } as NodeJS.ProcessEnv)
    ).toBe(13370);
  });
});

describe("loadTeamsCache", () => {
  test("returns null when cache missing", () => {
    const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-cache-"));
    expect(loadTeamsCache(tempDir)).toBeNull();
  });

  test("loads teams cache from disk", () => {
    const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-cache-"));
    const cacheFile = path.join(tempDir, "teams.json");
    const payload = {
      LEG: { id: "uuid-123", name: "Legion" },
    };
    fs.writeFileSync(cacheFile, JSON.stringify(payload, null, 2));

    const result = loadTeamsCache(tempDir);
    expect(result).toEqual(payload);
  });
});
