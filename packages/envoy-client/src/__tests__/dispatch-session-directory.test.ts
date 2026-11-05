import { describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { sessionDirectory, sessionStateRoot, writeAtomically } from "../dispatch-session-directory";

const root = () => mkdtempSync(join(tmpdir(), "dispatch-session-directory-"));

describe("session directory", () => {
  test("DISPATCH_STATE_DIR is the root, else XDG_STATE_HOME's dispatch; empty counts as unset", () => {
    expect(sessionStateRoot({ DISPATCH_STATE_DIR: "/state", XDG_STATE_HOME: "/xdg" })).toBe(
      "/state"
    );
    expect(sessionStateRoot({ DISPATCH_STATE_DIR: "", XDG_STATE_HOME: "/xdg" })).toBe(
      "/xdg/dispatch"
    );
    expect(sessionDirectory({ DISPATCH_STATE_DIR: "/state" }, "s1")).toBe("/state/sessions/s1");
  });

  test("a session id that could leave the state directory is refused", () => {
    expect(() => sessionDirectory({ DISPATCH_STATE_DIR: root() }, "../x")).toThrow();
    expect(() => sessionDirectory({ DISPATCH_STATE_DIR: root() }, "..")).toThrow();
  });
});

describe("writeAtomically", () => {
  test("replaces the file owner-only and leaves no temporary file", () => {
    const dir = root();
    const file = join(dir, "state.json");
    writeAtomically(file, "old");
    writeAtomically(file, "new");
    expect(readFileSync(file, "utf-8")).toBe("new");
    expect(statSync(file).mode & 0o777).toBe(0o600);
    expect(readdirSync(dir)).toEqual(["state.json"]);
  });

  test("a rename that fails throws and removes its temporary file", () => {
    const dir = root();
    // A directory at the path: the temporary file is written, the rename over it fails.
    const target = join(dir, "taken");
    mkdirSync(target);
    expect(() => writeAtomically(target, "text")).toThrow();
    expect(readdirSync(dir)).toEqual(["taken"]);
  });
});
