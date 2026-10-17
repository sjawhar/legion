import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// The `dispatch` entry is its own process in production, and every omp session reads it through a
// pipe: Bun's `process.exit` drops what a pipe has not yet taken, so the entry must wait for its
// writes. These tests run the real entry and read its output through a pipe.

const ENTRY = join(import.meta.dir, "..", "..", "bin", "dispatch.ts");
const PROJECTS = 4000;

const server = Bun.serve({
  port: 0,
  fetch(request) {
    if (new URL(request.url).pathname === "/api/v1/projects" && request.method === "GET") {
      return Response.json(
        Array.from({ length: PROJECTS }, (_, index) => ({
          key: `P${index}`,
          name: `Project number ${index}`,
          open_asks: 0,
          created_at: "2026-09-01T00:00:00Z",
        }))
      );
    }
    return Response.json({ error: "unexpected" }, { status: 500 });
  },
});

afterAll(() => server.stop(true));

async function runEntry(): Promise<{ code: number; stdout: Uint8Array; stderr: string }> {
  const state = mkdtempSync(join(tmpdir(), "dispatch-bin-"));
  // Bun.spawn's own stdout pipe drains fast enough to hide the loss, so the entry writes into a
  // shell pipe to `cat`, the way a bash tool captures it; pipefail keeps the entry's exit code.
  const child = Bun.spawn(["bash", "-c", 'set -o pipefail; bun "$0" projects | cat', ENTRY], {
    env: {
      PATH: process.env.PATH,
      HOME: state,
      DISPATCH_STATE_DIR: state,
      DISPATCH_HOST: "omp",
      DISPATCH_SESSION_ID: "ses-bin",
      DISPATCH_URL: `http://127.0.0.1:${server.port}`,
      DISPATCH_TOKEN: "test-token",
    },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).bytes(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { code, stdout, stderr };
}

describe("the dispatch entry", () => {
  test("writes all of its output to a pipe, however long", async () => {
    const { code, stdout, stderr } = await runEntry();
    expect(stderr).toBe("");
    expect(code).toBe(0);
    const text = new TextDecoder().decode(stdout);
    expect(stdout.length).toBeGreaterThan(64 * 1024);
    expect(text).toContain(`P${PROJECTS - 1}`);
    expect(text.endsWith("\n")).toBe(true);
  });
});
