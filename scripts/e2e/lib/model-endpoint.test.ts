import { afterAll, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// model_key_preflight against an endpoint shaped like the LiteLLM proxy the proofs' gateway is:
// /v1/models refuses a call with no key and answers one with the key, and the key's metadata is
// at /key/info (LiteLLM mounts its key management routes at the root, not under /v1, so
// /v1/key/info is not found). The preflight must name the key's alias there, which is the run's
// only record of which key the endpoint saw.
const lib = import.meta.dir;
const dir = mkdtempSync(join(tmpdir(), "model-endpoint-test."));
const key = "sk-gateway-test";
const server = Bun.serve({
  port: 0,
  hostname: "127.0.0.1",
  fetch(request) {
    const path = new URL(request.url).pathname;
    const keyed = request.headers.get("x-api-key") === key;
    if (path === "/v1/models") {
      return keyed
        ? Response.json({ data: [{ id: "claude-opus-5-5" }] })
        : new Response("", { status: 401 });
    }
    if (path === "/key/info") {
      return keyed
        ? Response.json({
            key: "hashed",
            info: { key_alias: "legion-infra" },
            key_alias: "legion-infra",
          })
        : new Response("", { status: 401 });
    }
    return new Response("not found", { status: 404 });
  },
});
afterAll(() => {
  server.stop(true);
  rmSync(dir, { recursive: true, force: true });
});

test("the preflight names the key's alias from a LiteLLM proxy's /key/info", async () => {
  const keyFile = join(dir, "key");
  writeFileSync(keyFile, key, { mode: 0o600 });
  // An async spawn: a synchronous one would block the server above, in this same process.
  const run = Bun.spawn(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
fail() { echo "FAIL: $*"; exit 1; }
note() { echo "note: $*"; }
source "$1/model-endpoint.sh"
model_key_preflight "$2" "$3" "$4/header" "the key file"`,
      "_",
      lib,
      `http://127.0.0.1:${server.port}`,
      keyFile,
      dir,
    ],
    { stdout: "pipe", stderr: "pipe" }
  );
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(run.stdout).text(),
    new Response(run.stderr).text(),
    run.exited,
  ]);
  const output = stdout + stderr;
  expect(exitCode).toBe(0);
  expect(output).toContain("the model key is legion-infra");
});
