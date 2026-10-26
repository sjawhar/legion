import { afterEach, describe, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync, unlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { messageAsEvents, startProxy } from "./recording-proxy";

type Seen = { path: string; key: string | null; body: string };
type Entry = Record<string, unknown> & { request_headers: Record<string, string> };

const cleanups: (() => void)[] = [];
afterEach(() => {
  for (const cleanup of cleanups.splice(0)) cleanup();
});

/** A stand-in provider that records what reached it and answers a fixed Messages response, as a
 * stream when the request asks for one. */
function upstream(seen: Seen[]) {
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request) {
      const url = new URL(request.url);
      const body = await request.text();
      seen.push({ path: url.pathname + url.search, key: request.headers.get("x-api-key"), body });
      const message = {
        id: "msg_1",
        type: "message",
        role: "assistant",
        content: [{ type: "text", text: "pong" }],
        usage: { input_tokens: 3, output_tokens: 1 },
      };
      if (JSON.parse(body || "{}").stream === true) {
        return new Response(messageAsEvents(message), {
          headers: { "content-type": "text/event-stream" },
        });
      }
      return Response.json(message);
    },
  });
  cleanups.push(() => server.stop(true));
  return server;
}

/** A proxy in front of the stand-in provider, its record and hold files in a scratch directory;
 * `recorded` resolves with the first entry the proxy appends, and `holding` once a request waits on
 * its hold file. */
function rig(seen: Seen[]) {
  const dir = mkdtempSync(join(tmpdir(), "recording-proxy-test."));
  const provider = upstream(seen);
  let entered: (entry: Entry) => void = () => {};
  let waiting: (label: string) => void = () => {};
  const recorded = new Promise<Entry>((resolve) => {
    entered = resolve;
  });
  const holding = new Promise<string>((resolve) => {
    waiting = resolve;
  });
  const proxy = startProxy({
    port: 0,
    upstream: `http://127.0.0.1:${provider.port}`,
    record: join(dir, "record.jsonl"),
    holdDir: dir,
    standInDir: dir,
    onRecord: (entry) => entered(entry as Entry),
    onHold: (label) => waiting(label),
  });
  cleanups.push(() => {
    proxy.stop(true);
    rmSync(dir, { recursive: true, force: true });
  });
  return { dir, base: `http://127.0.0.1:${proxy.port}`, recorded, holding };
}

const post = (url: string, body: unknown) =>
  fetch(url, {
    method: "POST",
    headers: { "content-type": "application/json", "x-api-key": "sk-secret-value" },
    body: JSON.stringify(body),
  });

describe("recording proxy", () => {
  test("forwards the request under its label's path and records both bodies with the key redacted", async () => {
    const seen: Seen[] = [];
    const { base, recorded } = rig(seen);
    const answer = await post(`${base}/tag/daemon/v1/messages?beta=true`, {
      model: "m",
      messages: [],
    });
    expect(await answer.json()).toMatchObject({ content: [{ text: "pong" }] });
    expect(seen).toEqual([
      {
        path: "/v1/messages?beta=true",
        key: "sk-secret-value",
        body: '{"model":"m","messages":[]}',
      },
    ]);
    const entry = await recorded;
    expect(entry).toMatchObject({
      label: "daemon",
      path: "/v1/messages",
      query: "?beta=true",
      mode: "forwarded",
      status: 200,
      request_body: { model: "m", messages: [] },
      response_body: { content: [{ type: "text", text: "pong" }] },
    });
    expect(entry.request_headers["x-api-key"]).toBe("<redacted>");
    expect(JSON.stringify(entry)).not.toContain("sk-secret-value");
  });

  test("keeps a streamed answer as the text it streamed, and the client gets the same stream", async () => {
    const seen: Seen[] = [];
    const { base, recorded } = rig(seen);
    const answer = await post(`${base}/tag/agent/v1/messages`, { stream: true });
    const streamed = await answer.text();
    expect(streamed).toContain('"text":"pong"');
    expect((await recorded).response_body).toBe(streamed);
  });

  test("answers from the label's stand-in file without reaching the provider", async () => {
    const seen: Seen[] = [];
    const { dir, base, recorded } = rig(seen);
    writeFileSync(
      join(dir, "loop.json"),
      JSON.stringify({
        type: "message",
        content: [{ type: "text", text: "not json" }],
        stop_reason: "end_turn",
        usage: { output_tokens: 2 },
      })
    );
    const answer = await post(`${base}/tag/loop/v1/messages`, { stream: false });
    expect(await answer.json()).toMatchObject({ content: [{ text: "not json" }] });
    expect(seen).toEqual([]);
    expect(await recorded).toMatchObject({ label: "loop", mode: "stand-in", status: 200 });
  });

  test("holds a labelled request above the released ordinal, naming it in the .held file", async () => {
    const seen: Seen[] = [];
    const { dir, base, recorded, holding } = rig(seen);
    writeFileSync(join(dir, "held"), "");
    const pending = post(`${base}/tag/held/v1/messages`, {});
    expect(await holding).toBe("held");
    expect(seen).toEqual([]);
    expect(readFileSync(join(dir, "held.held"), "utf8")).toBe("1\n");
    writeFileSync(join(dir, "held"), "1");
    await (await pending).json();
    expect(seen).toHaveLength(1);
    expect(await recorded).toMatchObject({ held: true, ordinal: 1 });
  });

  test("lets every request through once the hold file is removed", async () => {
    const seen: Seen[] = [];
    const { dir, base, holding } = rig(seen);
    writeFileSync(join(dir, "gone"), "0");
    const pending = post(`${base}/tag/gone/v1/messages`, {});
    await holding;
    unlinkSync(join(dir, "gone"));
    await (await pending).json();
    expect(seen).toHaveLength(1);
  });
});
