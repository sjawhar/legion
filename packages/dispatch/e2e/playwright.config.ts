import { fileURLToPath } from "node:url";
import { defineConfig, devices } from "@playwright/test";
import { baseUrl } from "./api";
import { harnessListeners } from "./harness-listeners";
import { plainHttpHost, plainHttpOrigin } from "./plain-http-origin";
import { takenPorts } from "./port-probe";

// The plain-HTTP project's origin is a proxy on PLAIN_HTTP_PORT, reached by a host name Chromium
// maps to loopback. The page is a non-loopback plain-HTTP origin, while the proxy forwards to the
// dev sign-in server under its own loopback origin. `plainHttpSpecs` are the files that project
// runs, the origin's own rows and the proxy's; `chromium` and `iphone` ignore them, since they
// open the plain-HTTP name, which only that project maps, and the origin rows' first assertion
// fails on loopback by design.
const plainHttpSpecs = /plain-http-(origin|proxy)\.e2e\.ts/;
const startsOwnServers = !process.env.PLAYWRIGHT_BASE_URL;

// The rows of e2e/harness-listeners.ts this run starts: every one for a local run, the `deployed`
// ones for a deployed run. The port probe and `webServer` both read `startedListeners`, so a
// listener is probed exactly when it is started.
const startedListeners = startsOwnServers
  ? harnessListeners
  : harnessListeners.filter((listener) => listener.deployed);

// `DISPATCH_E2E_REUSE_SERVERS=1` runs the suite against a harness the caller started and left
// listening on the five harness ports. Unset or empty starts this run's own servers and refuses a
// port already taken, because reusing a server this run did not start points `e2e/seed.ts`'s
// truncation at whatever database that server holds — another lane's. Any other value is refused
// rather than quietly read as "no".
function resolveReuseServers(): boolean {
  const requested = process.env.DISPATCH_E2E_REUSE_SERVERS;
  if (requested === undefined || requested === "") return false;
  if (requested === "1") return true;
  throw new Error(
    `DISPATCH_E2E_REUSE_SERVERS must be "1" or unset, not ${JSON.stringify(requested)}. ` +
      "Set it to 1 to run against a harness you started yourself, or leave it unset to have " +
      "this run start the harness."
  );
}

const reuseServers = resolveReuseServers();

// A listing run starts no web server: `listMode` builds only a load task and a report-begin task
// (`lib/runner/index.js:6946-6949`), where an ordinary run's in-process load sits at `:6952`,
// beneath `createGlobalSetupTasks`. This argv test is a CLI-shape proxy for that, not the rule
// itself, and it is deliberately the loose one: it takes any `--list` before a `--` separator,
// without asking whether the preceding token is an option that consumes a value. Reading `--list`
// as `--grep`'s value would be exact, but it also reads it as `--headed`'s value, and refusing a
// read-only listing because another lane holds a port is the error that costs a developer
// something. The other direction costs nothing: `--grep --list` is a real run that skips the
// probe and then meets Playwright's own refusal (`lib/runner/index.js:865`) — no spec runs and
// nothing is reused, only the message is less specific than ours. Enumerating Playwright's
// boolean flags to tell the two apart would pin this file to one version of its CLI. It does
// yield to a UI token, because `--ui`/`--ui-*` outrank the computed `listMode`
// (`lib/cli/testActions.js:52`, `:62`) and a UI session does start servers. The test server's own
// list path (`lib/runner/index.js:6763-6784`) carries no `--list` in argv, so it is probed;
// nothing in this repository uses it.
function isListMode(argv: readonly string[]): boolean {
  if (argv.some((arg) => arg === "--ui" || arg.startsWith("--ui-"))) return false;
  const separator = argv.indexOf("--");
  const scanned = separator === -1 ? argv : argv.slice(0, separator);
  return scanned.includes("--list");
}

// The probe runs while this module evaluates because no Playwright hook runs before the web
// servers: `webServer` entries become plugins, and `createGlobalSetupTasks` puts `globalSetup`
// after `createPluginSetupTasks` (`lib/runner/index.js:6321-6328`). Every worker re-imports this
// config once the servers are up (`lib/worker/workerProcessEntry.js:1481`), when the harness ports
// are legitimately taken by this run's own servers, and so does the out-of-process test loader
// (`lib/loader/loaderProcessEntry.js:16`) under the test server — UI mode and the editor
// extension; the plain CLI loads tests in-process (`lib/runner/index.js:6952`). Each is a
// `child_process.fork` whose stdio carries an `"ipc"` channel (`lib/runner/index.js:1915-1929`),
// so `process.send` is a function there and undefined in the CLI that starts the servers;
// `TEST_WORKER_INDEX` cannot discriminate them, because the loader never sets it.
if (!reuseServers && typeof process.send !== "function" && !isListMode(process.argv)) {
  const ports = await takenPorts(startedListeners);
  if (ports !== undefined) {
    const variables = startedListeners.map((listener) => listener.variable).join("/");
    const remedy = startsOwnServers
      ? `move this run to free ports with ${variables} and to its own DATABASE_URL, since the ` +
        "server already listening holds the database you named"
      : `move this run to free ports with ${variables}, and start the deployed server with the ` +
        "same FAKE_ENVOY_PORT, since its ENVOY_URL derives from it";
    throw new Error(
      `The Dispatch e2e harness cannot start: ${ports} already in use. Stop whatever listens ` +
        `there, or ${remedy}. To run against a harness you started yourself, set ` +
        "DISPATCH_E2E_REUSE_SERVERS=1."
    );
  }
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
  // Runs once the web servers are up and before any row: it refuses a target that does not read
  // this run's fake Envoy (e2e/preflight.ts).
  globalSetup: fileURLToPath(new URL("./preflight.ts", import.meta.url)),
  webServer: startedListeners.map(({ argv, port }) => ({
    command: argv.join(" "),
    port,
    reuseExistingServer: reuseServers,
  })),
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
      testMatch: /(agent-view|phone-conversation)\.e2e\.ts/,
      grep: /on a phone the live view|a document-scrolling route|covers none of|a capped model name/,
      use: { ...devices["iPhone 13"] },
    },
    // Firefox's native editing mishandles text typed over what follows a block's last line break,
    // the issue picker's keyboard-step rule rests on the engine's select dispatch, and offline
    // document recovery owns browser storage. Those specs, and the whole deep-links spec, run in
    // Firefox too.
    {
      name: "firefox",
      testMatch: /(code-line-replace|deep-links|keyboard-agents-picker|offline-edits)\.e2e\.ts/,
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
