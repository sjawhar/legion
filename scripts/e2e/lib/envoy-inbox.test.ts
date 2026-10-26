import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { startInbox } from "./envoy-inbox";

const cleanups: (() => void)[] = [];
afterEach(() => {
  for (const cleanup of cleanups.splice(0)) cleanup();
});

test("answers a listener's prompt delivery with 204 and records its session and text", async () => {
  const dir = mkdtempSync(join(tmpdir(), "envoy-inbox-test."));
  const record = join(dir, "inbox.jsonl");
  const inbox = startInbox(0, record);
  cleanups.push(() => {
    inbox.stop(true);
    rmSync(dir, { recursive: true, force: true });
  });
  const base = `http://127.0.0.1:${inbox.port}`;
  const delivered = await fetch(`${base}/session/rig-asker/prompt_async`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ parts: [{ type: "text", text: "the answer" }] }),
  });
  expect(delivered.status).toBe(204);
  expect((await fetch(`${base}/elsewhere`, { method: "POST" })).status).toBe(404);
  const [line] = readFileSync(record, "utf8").trim().split("\n");
  expect(JSON.parse(line ?? "")).toMatchObject({ session: "rig-asker", text: "the answer" });
});
