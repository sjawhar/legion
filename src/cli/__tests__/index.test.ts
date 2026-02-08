import { describe, test, expect, mock } from "bun:test";
import { parseArgs, type Command } from "../index";

describe("parseArgs", () => {
  test("parses start command with team", () => {
    const result = parseArgs(["start", "LEG"]);
    expect(result.command).toBe("start");
    expect(result.args.team).toBe("LEG");
    expect(result.args.workspace).toBe(process.cwd());
  });

  test("parses start command with workspace option", () => {
    const result = parseArgs(["start", "LEG", "--workspace", "/path/to/workspace"]);
    expect(result.command).toBe("start");
    expect(result.args.team).toBe("LEG");
    expect(result.args.workspace).toBe("/path/to/workspace");
  });

  test("parses start command with -w shorthand", () => {
    const result = parseArgs(["start", "LEG", "-w", "/path/to/workspace"]);
    expect(result.command).toBe("start");
    expect(result.args.team).toBe("LEG");
    expect(result.args.workspace).toBe("/path/to/workspace");
  });

  test("parses start command with state-dir option", () => {
    const result = parseArgs(["start", "LEG", "--state-dir", "/custom/state"]);
    expect(result.command).toBe("start");
    expect(result.args.team).toBe("LEG");
    expect(result.args.stateDir).toBe("/custom/state");
  });

  test("parses stop command", () => {
    const result = parseArgs(["stop", "LEG"]);
    expect(result.command).toBe("stop");
    expect(result.args.team).toBe("LEG");
  });

  test("parses status command", () => {
    const result = parseArgs(["status", "LEG"]);
    expect(result.command).toBe("status");
    expect(result.args.team).toBe("LEG");
  });

  test("parses attach command with issue identifier", () => {
    const result = parseArgs(["attach", "LEG-123"]);
    expect(result.command).toBe("attach");
    expect(result.args.issue).toBe("LEG-123");
  });

  test("parses teams command", () => {
    const result = parseArgs(["teams"]);
    expect(result.command).toBe("teams");
  });

  test("throws error for unknown command", () => {
    expect(() => parseArgs(["unknown"])).toThrow("Unknown command: unknown");
  });

  test("throws error for start without team", () => {
    expect(() => parseArgs(["start"])).toThrow("start requires a team argument");
  });

  test("throws error for stop without team", () => {
    expect(() => parseArgs(["stop"])).toThrow("stop requires a team argument");
  });

  test("throws error for status without team", () => {
    expect(() => parseArgs(["status"])).toThrow("status requires a team argument");
  });

  test("throws error for attach without issue", () => {
    expect(() => parseArgs(["attach"])).toThrow("attach requires an issue argument");
  });

  test("throws error for missing option value", () => {
    expect(() => parseArgs(["start", "LEG", "--workspace"])).toThrow(
      "Option --workspace requires a value"
    );
  });

  test("throws error for no command", () => {
    expect(() => parseArgs([])).toThrow("No command provided");
  });
});
