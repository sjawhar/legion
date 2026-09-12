import { afterEach, describe, expect, it } from "bun:test";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { materializeDeploymentInstructions } from "../deployment-instructions";

const tempDirs: string[] = [];

async function scratchDir(): Promise<string> {
  const dir = await mkdtemp(path.join(os.tmpdir(), "legion-deployment-instructions-"));
  tempDirs.push(dir);
  return dir;
}

afterEach(async () => {
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
});

describe("materializeDeploymentInstructions", () => {
  it("writes the header naming the verbatim project followed by the file's exact content", async () => {
    const dir = await scratchDir();
    const source = path.join(dir, "ops", "deployment.md");
    await mkdir(path.dirname(source), { recursive: true });
    const content =
      "## Required checks\n\n- `pr-checks-result`\n\nAsk `notifications.role.librarian` first.\n";
    await writeFile(source, content, "utf8");
    const stateDir = path.join(dir, "state");
    await mkdir(stateDir);

    const target = await materializeDeploymentInstructions(source, stateDir, "sjawhar/legion");

    expect(target).toBe(path.join(stateDir, "deployment-instructions.md"));
    expect(await readFile(target, "utf8")).toBe(
      `# Deployment instructions (sjawhar/legion)\n\n${content}`
    );
  });

  it("refuses a missing file, naming the resolved path", async () => {
    const dir = await scratchDir();
    const source = path.join(dir, "absent.md");
    await expect(materializeDeploymentInstructions(source, dir, "acme/1")).rejects.toThrow(
      `instructions file ${source} could not be read`
    );
  });

  it("refuses a directory, naming the resolved path", async () => {
    const dir = await scratchDir();
    const source = path.join(dir, "instructions-dir");
    await mkdir(source);
    await expect(materializeDeploymentInstructions(source, dir, "acme/1")).rejects.toThrow(
      `instructions file ${source} could not be read`
    );
  });

  it("refuses a whitespace-only file instead of appending an empty fragment", async () => {
    const dir = await scratchDir();
    const source = path.join(dir, "blank.md");
    await writeFile(source, " \n\t\n", "utf8");
    await expect(materializeDeploymentInstructions(source, dir, "acme/1")).rejects.toThrow(
      `instructions file ${source} is empty`
    );
    await expect(readFile(path.join(dir, "deployment-instructions.md"))).rejects.toThrow();
  });
});
