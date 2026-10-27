import { expect, test } from "bun:test";

// The run record uploader is a standard-library Python script the deployment runs as root
// (README.md, "Run records"); its tests are Python's unittest, run here so `bun test` covers them.
test("the run record uploader's own tests pass", () => {
  const result = Bun.spawnSync(["python3", "-m", "unittest", "-v", "run_record_upload_test"], {
    cwd: import.meta.dir,
    env: { ...process.env, PYTHONDONTWRITEBYTECODE: "1" },
    stdout: "pipe",
    stderr: "pipe",
  });
  const output = `${result.stdout.toString()}${result.stderr.toString()}`;
  if (result.exitCode !== 0) {
    throw new Error(`python3 -m unittest exited ${result.exitCode}:\n${output}`);
  }
  expect(output).toMatch(/Ran [1-9][0-9]* tests? in/);
}, 120_000);
