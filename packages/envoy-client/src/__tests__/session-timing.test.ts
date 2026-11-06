import { expect, test } from "bun:test";
import { mkdtempSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import {
  LISTENER_CALL_ATTEMPTS,
  LISTENER_CALL_RETRY_DELAY_MS,
  LISTENER_CALL_TIMEOUT_MS,
  SESSION_HEARTBEAT_MS,
} from "@legion/contracts";
import { readEnvoyConfig } from "../envoy-config";
import { envoySettings } from "../envoy-settings";
import { resolveCallTiming } from "../transport";

const listener = {
  enabled: true,
  url: "http://listener.internal.example:9020/",
  natsUrls: ["nats://nats.internal.example:4222"],
  token: "file-token",
  natsSeed: "SUFILESEED",
};

/** A configuration naming that listener, read from a file with no checkout file beside it. */
function listenerConfig() {
  const dir = mkdtempSync(path.join(os.tmpdir(), "session-timing-"));
  const file = path.join(dir, "envoy.json");
  writeFileSync(file, JSON.stringify({ envoy: listener }));
  return readEnvoyConfig(file, { HOME: dir }, { cwd: dir });
}

/** The declarations the renewal window is weighed over, as the client resolves them with nothing
 * of its own: the interval a pane refreshes its listener registration at, and what one whole
 * listener call costs. Three intervals plus one call fall strictly inside the live window,
 * which holds only while the client reads these numbers rather than numbers of its own. */
test("CLIENTTEST the client resolves the declared refresh interval and call budget", () => {
  const settings = envoySettings({}, listenerConfig());
  if (!settings.enabled) {
    throw new Error(`Envoy resolved as off: ${JSON.stringify(settings)}`);
  }
  expect(settings.heartbeatMs).toBe(SESSION_HEARTBEAT_MS);

  const timing = resolveCallTiming({});
  expect(timing).toEqual({
    timeoutMs: LISTENER_CALL_TIMEOUT_MS,
    attempts: LISTENER_CALL_ATTEMPTS,
    retryDelayMs: LISTENER_CALL_RETRY_DELAY_MS,
  });

  const callBudgetMs =
    timing.attempts * timing.timeoutMs + (timing.attempts - 1) * timing.retryDelayMs;
  const ms = (value: number) => value.toLocaleString("en-US");
  console.log(
    `refresh interval ${ms(settings.heartbeatMs)} ms; one listener call is ${ms(timing.attempts)} attempts of ${ms(timing.timeoutMs)} ms with ${ms(timing.retryDelayMs)} ms between them, a budget of ${ms(callBudgetMs)} ms`
  );
});
