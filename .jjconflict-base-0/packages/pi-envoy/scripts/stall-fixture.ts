// Observation fixture for `scripts/smoke-registration-stall.sh` only. No deployment loads it: it
// is not in this package's `omp.extensions`, it is not bundled, and the smoke passes it to `omp`
// with `-e` ahead of the plugin so this module body replaces `globalThis.fetch` before the plugin
// captures it (`createEnvoyClient({ fetch })`).
import { appendFileSync } from "node:fs";
import { logger } from "@oh-my-pi/pi-utils";

const logPath = process.env.STALL_FIXTURE_LOG ?? "";
/** 1-based registration to wedge; unset or 0 wedges nothing. */
const wedgeAt = Number(process.env.STALL_FIXTURE_WEDGE_AT ?? "0");

const record = (line: string): void => {
  if (logPath === "") return;
  appendFileSync(logPath, `${line}\n`);
};

const realFetch = globalThis.fetch;
type FetchInput = Parameters<typeof fetch>[0];
let registrations = 0;
globalThis.fetch = (async (input: FetchInput, init?: RequestInit): Promise<Response> => {
  const method = init?.method ?? (input instanceof Request ? input.method : "GET");
  const href = input instanceof Request ? input.url : String(input);
  // Relative hrefs cannot reach the listener, and the base keeps `new URL` from throwing on one.
  const path = new URL(href, "http://fixture.invalid").pathname;
  if (method.toUpperCase() !== "POST" || path !== "/v1/interests/subscribe") {
    return await realFetch(input, init);
  }
  registrations += 1;
  if (registrations === wedgeAt) {
    record(`registration ${registrations} wedged`);
    // The never-settling await the plugin cannot produce on its own: the listener never sees this
    // post, and the request's own abort signal is deliberately never read.
    await new Promise<never>(() => {});
  }
  record(`registration ${registrations}`);
  return await realFetch(input, init);
}) as typeof fetch;

logger.registerLogSink((event) => {
  if (event.level === "warn") record(`warn ${event.message}`);
});

/** Loaded before the plugin; the wrapper and the sink above are the whole fixture. */
export default function stallFixture(): void {
  record("fixture loaded");
}
