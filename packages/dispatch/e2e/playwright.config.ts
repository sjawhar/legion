import { connect } from "node:net";
import { fileURLToPath } from "node:url";
import { defineConfig, devices } from "@playwright/test";

// The three harness ports are shared inputs: `run-server.sh` and the e2e helpers resolve the same
// variables. A value that is not a port number is refused here, naming its variable, because every
// later consumer turns it into something that names nothing — `net.connect` raises `RangeError:
// Port should be >= 0 and < 65536` on `NaN`, and a `webServer` entry would wait on it.
function harnessPort(variable: string, resolved: string): number {
  const port = Number(resolved);
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    throw new Error(`${variable} must be a port number, not ${JSON.stringify(resolved)}.`);
  }
  return port;
}

const e2ePort = process.env.DISPATCH_E2E_PORT || "8777";
const e2ePortNumber = harnessPort("DISPATCH_E2E_PORT", e2ePort);
const baseURL = process.env.PLAYWRIGHT_BASE_URL ?? `http://127.0.0.1:${e2ePort}`;
const startsOwnServers = !process.env.PLAYWRIGHT_BASE_URL;
const fakeEnvoy = fileURLToPath(new URL("./fake-envoy.ts", import.meta.url));
const fakeEnvoyPort = harnessPort("FAKE_ENVOY_PORT", process.env.FAKE_ENVOY_PORT ?? "9021");
const fakeGithub = fileURLToPath(new URL("./fake-github.ts", import.meta.url));
const fakeGithubPort = harnessPort("FAKE_GITHUB_PORT", process.env.FAKE_GITHUB_PORT ?? "9022");
const runServer = fileURLToPath(new URL("./run-server.sh", import.meta.url));

// `DISPATCH_E2E_REUSE_SERVERS=1` runs the suite against a harness the caller started and left
// listening on the three harness ports. Unset or empty starts this run's own servers and refuses a
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

const harnessPorts = [
  { variable: "DISPATCH_E2E_PORT", port: e2ePortNumber },
  { variable: "FAKE_ENVOY_PORT", port: fakeEnvoyPort },
  { variable: "FAKE_GITHUB_PORT", port: fakeGithubPort },
];

// Playwright's own port predicate (playwright@1.63.0 `lib/runner/index.js:958-977`): a port counts
// as used when either `127.0.0.1` or `::1` accepts a connection. A probe that dialled only
// `127.0.0.1` would miss a listener bound on `::1` alone — what a docker-published port binds —
// and leave that case to Playwright's backstop, which names no variable.
function isPortUsed(port: number): Promise<boolean> {
  const dial = (host: string) => {
    const { promise, resolve } = Promise.withResolvers<boolean>();
    const connection = connect(port, host)
      .on("error", () => resolve(false))
      .on("connect", () => {
        connection.end();
        resolve(true);
      });
    return promise;
  };
  const { promise, resolve } = Promise.withResolvers<boolean>();
  let pending = 2;
  const onResult = (used: boolean) => {
    if (used) resolve(true);
    else if (--pending === 0) resolve(false);
  };
  void dial("127.0.0.1").then(onResult);
  void dial("::1").then(onResult);
  return promise;
}

// The probe runs while this module evaluates because no Playwright hook runs before the web
// servers: `webServer` entries become plugins, and `createGlobalSetupTasks` puts `globalSetup`
// after `createPluginSetupTasks` (`lib/runner/index.js:6321-6328`). Two things must not reach it.
// Every worker re-imports this config once the servers are up
// (`lib/worker/workerProcessEntry.js:1481`), when the harness ports are legitimately taken by this
// run's own servers, and so does the out-of-process test loader
// (`lib/loader/loaderProcessEntry.js:16`) under the test server — UI mode and the editor
// extension; the plain CLI loads tests in-process (`lib/runner/index.js:6947`). Each is a
// `child_process.fork` whose stdio carries an `"ipc"` channel (`lib/runner/index.js:1915-1929`),
// so `process.send` is a function there and undefined in the CLI that starts the servers;
// `TEST_WORKER_INDEX` cannot discriminate them, because the loader never sets it. `--list` starts
// no web server at all: `listMode` builds only a load task and a report-begin task
// (`lib/runner/index.js:6946-6949`), with no `createGlobalSetupTasks`.
if (
  startsOwnServers &&
  !reuseServers &&
  typeof process.send !== "function" &&
  !process.argv.includes("--list")
) {
  const used = await Promise.all(harnessPorts.map((entry) => isPortUsed(entry.port)));
  const taken = harnessPorts.filter((_, index) => used[index]);
  if (taken.length > 0) {
    const ports = taken.map((entry) => `${entry.port} (${entry.variable})`).join(", ");
    throw new Error(
      `The Dispatch e2e harness cannot start: ${ports} already in use. Stop whatever listens ` +
        "there, or move this run to free ports with DISPATCH_E2E_PORT/FAKE_ENVOY_PORT/" +
        "FAKE_GITHUB_PORT. To run against a harness you started yourself, set " +
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
    baseURL,
    headless: true,
    trace: "retain-on-failure",
  },
  ...(startsOwnServers
    ? {
        webServer: [
          {
            command: `bun ${fakeEnvoy}`,
            port: fakeEnvoyPort,
            reuseExistingServer: reuseServers,
          },
          {
            command: `bun ${fakeGithub}`,
            port: fakeGithubPort,
            reuseExistingServer: reuseServers,
          },
          {
            command: `bash ${runServer}`,
            port: e2ePortNumber,
            reuseExistingServer: reuseServers,
          },
        ],
      }
    : {}),
  projects: [
    { name: "chromium", use: { ...devices["Desktop Chrome"] } },
    { name: "iphone", use: { ...devices["iPhone 13"], browserName: "chromium" } },
    // A caret beside a collaborator's cursor behaves per engine, so that spec also runs in WebKit.
    {
      name: "webkit",
      testMatch: /collab-cursor\.e2e\.ts/,
      use: { ...devices["Desktop Safari"] },
    },
    // The live view's phone layout (its keyboard cap, gutter and scroll locks) also runs in WebKit,
    // the engine iOS Safari uses; only those rows, since the rest of the spec is engine-agnostic.
    {
      name: "webkit-iphone",
      testMatch: /agent-view\.e2e\.ts/,
      grep: /on a phone the live view|a document-scrolling route/,
      use: { ...devices["iPhone 13"] },
    },
    // Firefox's native editing mishandles text typed over what follows a block's last line break,
    // so that spec also runs in Firefox.
    {
      name: "firefox",
      testMatch: /code-line-replace\.e2e\.ts/,
      use: { ...devices["Desktop Firefox"] },
    },
  ],
});
