import { fileURLToPath } from "node:url";
import { defineConfig, devices } from "@playwright/test";
import { baseUrl } from "./api";
import { targetedListeners } from "./harness-listeners";
import { harnessPorts, harnessVariables } from "./harness-ports";
import { plainHttpHost, plainHttpOrigin } from "./plain-http-origin";

// The plain-HTTP project's origin is a proxy on PLAIN_HTTP_PORT, reached by a host name Chromium
// maps to loopback. The page is a non-loopback plain-HTTP origin, while the proxy forwards to the
// dev sign-in server under its own loopback origin. `plainHttpSpecs` are the files that project
// runs, the origin's own rows and the proxy's; `chromium` and `iphone` ignore them, since they
// open the plain-HTTP name, which only that project maps, and the origin rows' first assertion
// fails on loopback by design.
const plainHttpSpecs = /plain-http-(origin|proxy)\.e2e\.ts/;

// The rows of e2e/harness-listeners.ts this run targets, as the environment names them when this
// config evaluates.
const targeted = targetedListeners();

// `DISPATCH_E2E_REUSE_SERVERS=1` says the harness ports name servers that are already running:
// the ones e2e/run.ts started, which sets it for the command it runs, or a harness a person
// started and left listening. Any other value is refused rather than quietly read as "no".
function resolveReuseServers(): boolean {
  const requested = process.env.DISPATCH_E2E_REUSE_SERVERS;
  if (requested === undefined || requested === "") return false;
  if (requested === "1") return true;
  throw new Error(
    `DISPATCH_E2E_REUSE_SERVERS must be "1" or unset, not ${JSON.stringify(requested)}. ` +
      "Run the suite through e2e/run.ts, which sets it for the harness it starts, or set it to 1 " +
      "to run against a harness you started yourself."
  );
}

// This config starts no server. e2e/run.ts starts the harness (e2e/harness-start.ts) on ports
// the OS chooses, exports the ports the servers report, and only then runs Playwright, so this
// module, every worker that re-imports it, and every helper that reads a port at import see the
// real ports when they evaluate. Without it, the run targets servers a person started: every
// port set and DISPATCH_E2E_REUSE_SERVERS=1, since e2e/seed.ts truncates the database those
// servers hold. Anything else is refused here, before a single spec runs; `harnessPorts()` also
// refuses a malformed or duplicated port.
const runThroughWrapper =
  "bun e2e/run.ts -- bunx playwright test --config e2e/playwright.config.ts [args] (`bun run " +
  "e2e` builds the SPA and runs the same)";
const ports = harnessPorts();
const unset = targeted.filter((listener) => ports[listener.key].port === 0);
if (unset.length > 0) {
  throw new Error(
    `The Dispatch e2e harness is not running: ${unset.map((listener) => harnessVariables[listener.key]).join(", ")} ` +
      `${unset.length > 1 ? "are" : "is"} unset. This config starts no server; run it through ` +
      `e2e/run.ts, which starts the harness on ports the OS chooses and exports them: ` +
      `${runThroughWrapper}. To run against servers you started yourself, set ` +
      `${targeted.map((listener) => harnessVariables[listener.key]).join(", ")} and ` +
      "DISPATCH_E2E_REUSE_SERVERS=1."
  );
}
if (!resolveReuseServers()) {
  const targets = targeted
    .map((listener) => `${ports[listener.key].port} (${harnessVariables[listener.key]})`)
    .join(", ");
  throw new Error(
    `This config starts no server, so it would run against whatever listens on ${targets}. Run ` +
      `it through e2e/run.ts, which starts the harness: ${runThroughWrapper}. Or set ` +
      "DISPATCH_E2E_REUSE_SERVERS=1 to run against servers you started yourself, whose database " +
      "this run truncates."
  );
}

// The default wait for asynchronous server and rendering readiness. A spec only sets its own
// timeout when that deadline is its observable contract (the keyboard chord-expiry test).
const expectTimeout = 15_000;

export default defineConfig({
  testDir: ".",
  testMatch: /.*\.e2e\.ts/,
  outputDir: "test-results",
  fullyParallel: false,
  workers: 1,
  reporter: "list",
  expect: { timeout: expectTimeout },
  use: {
    ...devices["Desktop Chrome"],
    baseURL: baseUrl,
    headless: true,
    trace: "retain-on-failure",
  },
  // Runs before any row, against the servers the checks above require: it refuses a target that
  // does not read this run's fake Envoy (e2e/preflight.ts).
  globalSetup: fileURLToPath(new URL("./preflight.ts", import.meta.url)),
  projects: [
    { name: "chromium", testIgnore: plainHttpSpecs, use: { ...devices["Desktop Chrome"] } },
    {
      name: "iphone",
      testIgnore: plainHttpSpecs,
      use: { ...devices["iPhone 13"], browserName: "chromium" },
    },
    // A caret beside a collaborator's cursor behaves per engine, the issue picker's keyboard-step
    // rule rests on each engine dispatching a closed select's `change` in the key's own task, deep
    // links meet each engine's chunk cancellation and the margin hold's frame and scroll order,
    // and IndexedDB-backed document recovery must run in the engines people use. So those specs
    // also run in WebKit.
    {
      name: "webkit",
      testMatch: /(collab-cursor|deep-links|keyboard-agents-picker|offline-edits)\.e2e\.ts/,
      use: { ...devices["Desktop Safari"] },
    },
    // The live view's phone layout (its keyboard cap, gutter and scroll locks), its model
    // pill's ellipsis (the two engines lay out a flex child's min-width differently), and what
    // the Conversation's floating pills cover on a phone, also run in WebKit, the engine iOS
    // Safari uses; only those rows, since the rest of each spec is engine-agnostic.
    {
      name: "webkit-iphone",
      testMatch: /(agent-view|phone-conversation|delivery-live)\.e2e\.ts/,
      grep: /on a phone the live view|a document-scrolling route|covers none of|a capped model name|on a phone the delivery page/,
      use: { ...devices["iPhone 13"] },
    },
    // Firefox's native editing mishandles text typed over what follows a block's last line break,
    // the issue picker's keyboard-step rule rests on the engine's select dispatch, offline
    // document recovery owns browser storage, and a click on a link inside the editor meets the
    // engine's focus rule (Firefox focuses the editing host, not the link). Those specs, and the
    // whole deep-links spec, run in Firefox too.
    {
      name: "firefox",
      testMatch:
        /(code-line-replace|deep-links|keyboard-agents-picker|offline-edits|picture-link-click)\.e2e\.ts/,
      use: { ...devices["Desktop Firefox"] },
    },

    {
      name: "firefox-download-navigation",
      testMatch: /shell\.e2e\.ts/,
      grep: /a chunk that fails after a Download link was followed reloads the page/,
      use: { ...devices["Desktop Firefox"] },
    },
    {
      name: "chromium-plain-http",
      testMatch: plainHttpSpecs,
      use: {
        ...devices["Desktop Chrome"],
        baseURL: plainHttpOrigin,
        launchOptions: { args: [`--host-resolver-rules=MAP ${plainHttpHost} 127.0.0.1`] },
      },
    },
  ],
});
