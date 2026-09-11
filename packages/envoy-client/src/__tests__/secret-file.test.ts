import { describe, expect, test } from "bun:test";
import { mkdtempSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { readSecretFile } from "../secret-file";

describe("readSecretFile", () => {
  test("returns the file contents trimmed", () => {
    const dir = mkdtempSync(path.join(os.tmpdir(), "secret-file-"));
    const file = path.join(dir, "token");
    writeFileSync(file, "  the-token\n");
    expect(readSecretFile("DISPATCH_TOKEN_FILE", file)).toBe("the-token");
  });

  test("throws naming the variable and path for an unreadable or empty file", () => {
    const dir = mkdtempSync(path.join(os.tmpdir(), "secret-file-"));
    const missing = path.join(dir, "missing");
    const empty = path.join(dir, "empty");
    writeFileSync(empty, "\n");
    expect(() => readSecretFile("DISPATCH_TOKEN_FILE", missing)).toThrow(
      `DISPATCH_TOKEN_FILE names ${missing}, which could not be read`
    );
    expect(() => readSecretFile("DISPATCH_TOKEN_FILE", empty)).toThrow(
      `DISPATCH_TOKEN_FILE names ${empty}, which is empty`
    );
  });
});
