import { spawn } from "node:child_process";
import { once } from "node:events";
import { createServer, request as httpRequest } from "node:http";
import type { AddressInfo } from "node:net";
import { setTimeout as sleep } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { expect, test } from "@playwright/test";

import type { FakeInterest, FakeSession } from "./agents";

// The fake Envoy is the harness's own fixture, so this row starts a second one on a port of its
// own rather than reseeding the listener every other row in the run shares.
const fakeEnvoyModule = fileURLToPath(new URL("./fake-envoy.ts", import.meta.url));

test("an interests seed whose body arrives during a session reseed is kept", async () => {
  test.skip(test.info().project.name !== "chromium", "the fixture has no browser to vary");
  const probe = createServer().listen(0, "127.0.0.1");
  await once(probe, "listening");
  const port = (probe.address() as AddressInfo).port;
  probe.close();
  await once(probe, "close");
  const fake = spawn("bun", [fakeEnvoyModule], {
    env: { ...process.env, FAKE_ENVOY_PORT: String(port) },
    stdio: ["ignore", "pipe", "inherit"],
  });
  const exited = once(fake, "exit");
  try {
    const ready = Promise.withResolvers<void>();
    let output = "";
    fake.stdout.setEncoding("utf8");
    fake.stdout.on("data", (chunk: string) => {
      output += chunk;
      if (output.includes("fake envoy listener on")) ready.resolve();
    });
    fake.once("exit", (code) => {
      ready.reject(new Error(`the fake Envoy exited ${code} before it listened: ${output}`));
    });
    fake.once("error", ready.reject);
    await ready.promise;
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
    const reseed = await fetch(`${origin}/__fixture/sessions`, {
      body: JSON.stringify(sessions),
      headers: { "Content-Type": "application/json" },
      method: "PUT",
    });
    expect(reseed.status).toBe(200);
    interestsPut.end(body);
    expect(await answered.promise).toBe(200);

    const stored = await fetch(`${origin}/v1/interests/`);
    expect(await stored.json()).toEqual(interests);
  } finally {
    fake.kill();
    await exited;
  }
});
