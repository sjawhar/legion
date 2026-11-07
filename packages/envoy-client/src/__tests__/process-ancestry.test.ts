import { expect, test } from "bun:test";
import { isAncestor } from "../process-ancestry";

test("isAncestor finds this process and its parent, and not a process outside its chain", () => {
  expect(isAncestor(String(process.pid))).toBe(true);
  expect(isAncestor(String(process.ppid))).toBe(true);
  expect(isAncestor("1")).toBe(false);
  const child = Bun.spawnSync([
    process.execPath,
    "-e",
    "process.stdout.write(String(process.pid))",
  ]);
  expect(isAncestor(child.stdout.toString())).toBe(false);
});
