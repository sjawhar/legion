import { expect, test } from "bun:test";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { request as httpRequest } from "node:http";
import { setTimeout as sleep } from "node:timers/promises";
import { fileURLToPath } from "node:url";

import type { FakeInterest, FakeSession } from "../agents";
import { withoutHarnessPort } from "../harness-ports";

// The fake Envoy is the browser harness's own fixture (e2e/fake-envoy.ts) and no browser varies
// it, so this test runs here: it starts a fake of its own on a port of its own rather than
// reseeding the listener a browser run shares. With the fake Envoy port unset under every name
// this lane carries it under, the fake binds a port the OS chooses, and the test reads it from
// the fake's listening line. The fake is a `bun` process of its own, as the harness starts it,
// and this file asks with `Bun.fetch`: the test preload (web/src/__tests__/setup.ts) puts
// happy-dom's `fetch` on globalThis, for the reason e2e/__tests__/round-robin-proxy.test.ts
// gives.
const fakeEnvoyModule = fileURLToPath(new URL("../fake-envoy.ts", import.meta.url));

test("an interests seed whose body arrives during a session reseed is kept", async () => {
  const env = withoutHarnessPort("fakeEnvoy");
  const fake = spawn("bun", [fakeEnvoyModule], { env, stdio: ["ignore", "pipe", "inherit"] });
  const exited = once(fake, "exit");
  try {
    const ready = Promise.withResolvers<number>();
    let output = "";
    fake.stdout.setEncoding("utf8");
    fake.stdout.on("data", (chunk: string) => {
      output += chunk;
      const listening = /fake envoy listener on 127\.0\.0\.1:(\d+)\n/.exec(output);
      if (listening !== null) ready.resolve(Number(listening[1]));
    });
    fake.once("exit", (code) => {
      ready.reject(new Error(`the fake Envoy exited ${code} before it listened: ${output}`));
    });
    fake.once("error", ready.reject);
    const port = await ready.promise;
    const origin = `http://127.0.0.1:${port}`;
    // The interests PUT's headers go first and its body last, with a whole session reseed in
    // between: `conversation.e2e.ts` and `inbox.e2e.ts` send the two seeds at once, and a client
    // that splits a request's headers from its body puts the reseed inside the handler's await.
    const interests: FakeInterest[] = [{ session_id: "e2e-interest", topics: ["dispatch.CORE"] }];
    const body = JSON.stringify(interests);
    const answered = Promise.withResolvers<number>();
    const interestsPut = httpRequest(
      {
        headers: { "Content-Length": Buffer.byteLength(body), "Content-Type": "application/json" },
        host: "127.0.0.1",
        method: "PUT",
        path: "/__fixture/interests",
        port,
      },
      (response) => {
        response.resume();
        response.on("end", () => answered.resolve(response.statusCode ?? 0));
      }
    ).on("error", answered.reject);
    interestsPut.flushHeaders();
    // Long enough for the fixture to have entered the handler and be waiting on the body. A
    // reseed that lands before the handler starts cannot show the loss, so too short a wait
    // passes a broken fixture, never fails a sound one.
    await sleep(100);
    const sessions: FakeSession[] = [{ session_id: "e2e-reseeded", title: "Reseeded" }];
    const reseed = await Bun.fetch(`${origin}/__fixture/sessions`, {
      body: JSON.stringify(sessions),
      headers: { "Content-Type": "application/json" },
      method: "PUT",
    });
    expect(reseed.status).toBe(200);
    interestsPut.end(body);
    expect(await answered.promise).toBe(200);

    const stored = await Bun.fetch(`${origin}/v1/interests/`);
    expect(await stored.json()).toEqual(interests);
  } finally {
    fake.kill();
    await exited;
  }
});
