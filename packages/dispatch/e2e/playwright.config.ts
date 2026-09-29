import { connect } from "node:net";
import { fileURLToPath } from "node:url";
import { defineConfig, devices } from "@playwright/test";
import { dispatchPort, fakeEnvoyPort, fakeGithubPort, harnessPorts } from "./harness-ports";

const baseURL = process.env.PLAYWRIGHT_BASE_URL ?? `http://127.0.0.1:${dispatchPort}`;
const startsOwnServers = !process.env.PLAYWRIGHT_BASE_URL;
const fakeEnvoy = fileURLToPath(new URL("./fake-envoy.ts", import.meta.url));
const fakeGithub = fileURLToPath(new URL("./fake-github.ts", import.meta.url));
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

// Two variables naming one port pass the probe below — each port is free on its own — and then
// reach Playwright's own refusal, which names no variable. Refused here rather than inside the
// probe's gate, so a `--list` run and the deployed path catch it too.
const collisions = [...new Set(harnessPorts.map((entry) => entry.port))]
  .map((port) => ({
    port,
    variables: harnessPorts.filter((entry) => entry.port === port).map((entry) => entry.variable),
  }))
  .filter((collision) => collision.variables.length > 1);
if (collisions.length > 0) {
  const named = collisions
    .map(({ port, variables }) => {
      const names =
        variables.length > 2
          ? `${variables.slice(0, -1).join(", ")} and ${variables[variables.length - 1]}`
          : variables.join(" and ");
      return `${names} name port ${port}`;
    })
    .join("; ");
  throw new Error(
    `The Dispatch e2e harness cannot start: ${named}. Give each harness server its own port.`
  );
}

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

// A listing run starts no web server: `listMode` builds only a load task and a report-begin task
// (`lib/runner/index.js:6946-6949`), where an ordinary run's in-process load sits at `:6952`,
// beneath `createGlobalSetupTasks`. This argv test is a CLI-shape proxy for that, not the rule
// itself: it counts `--list` only before a `--` separator and never as the value of the preceding
// option, and it yields to a UI token, because `--ui`/`--ui-*` outrank the computed `listMode`
// (`lib/cli/testActions.js:52`, `:62`). Both of its errors are safe. A false skip leaves
// Playwright's own refusal (`lib/runner/index.js:865`), never a reuse; a false probe only replaces
// a listing with our named refusal — which is also what the test server's own list path
// (`lib/runner/index.js:6763-6784`) gets, since it carries no `--list` in argv and nothing in this
// repository uses it.
function isListMode(argv: readonly string[]): boolean {
  if (argv.some((arg) => arg === "--ui" || arg.startsWith("--ui-"))) return false;
  const separator = argv.indexOf("--");
  const scanned = separator === -1 ? argv : argv.slice(0, separator);
  return scanned.some((arg, index) => {
    if (arg !== "--list") return false;
    const previous = scanned[index - 1];
    return previous === undefined || !previous.startsWith("-") || previous.includes("=");
  });
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
if (
  startsOwnServers &&
  !reuseServers &&
  typeof process.send !== "function" &&
  !isListMode(process.argv)
) {
  const used = await Promise.all(harnessPorts.map((entry) => isPortUsed(entry.port)));
  const taken = harnessPorts.filter((_, index) => used[index]);
  if (taken.length > 0) {
    const ports = taken.map((entry) => `${entry.port} (${entry.variable})`).join(", ");
    throw new Error(
      `The Dispatch e2e harness cannot start: ${ports} already in use. Stop whatever listens ` +
        "there, or move this run to free ports with DISPATCH_E2E_PORT/FAKE_ENVOY_PORT/" +
        "FAKE_GITHUB_PORT and to its own DATABASE_URL, since the server already listening still " +
        "holds the database you named. To run against a harness you started yourself, set " +
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
            port: dispatchPort,
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
