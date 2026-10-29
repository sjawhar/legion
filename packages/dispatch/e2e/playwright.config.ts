import { fileURLToPath } from "node:url";
import { defineConfig, devices } from "@playwright/test";
import { baseUrl } from "./api";
import { targetedListeners } from "./harness-listeners";
import { harnessPorts, harnessVariables } from "./harness-ports";
import { harnessProcessCount } from "./harness-processes";
import { harnessSignIn } from "./harness-signin";
import { plainHttpHost, plainHttpOrigin } from "./plain-http-origin";

// The plain-HTTP project's origin is a proxy on PLAIN_HTTP_PORT, reached by a host name Chromium
// maps to loopback. The page is a non-loopback plain-HTTP origin, while the proxy forwards to the
// dev sign-in server under its own loopback origin. `plainHttpSpecs` are the files that project
// runs, the origin's own rows and the proxy's; `chromium` and `iphone` ignore them, since they
// open the plain-HTTP name, which only that project maps, and the origin rows' first assertion
// fails on loopback by design.
const plainHttpSpecs = /plain-http-(origin|proxy)\.e2e\.ts/;
// The `iphone` project runs only tests tagged `@phone`; which tests take the tag is in
// `packages/dispatch/AGENTS.md`, "Phone acceptance". An untagged test drops out with no failure.
// The regex is anchored so `@phone` matches only as a whole whitespace-separated token, never
// inside a longer one such as `@phones` or `x@phone`.
const phoneTagged = /(^|\s)@phone(\s|$)/;
// Specs of two Dispatch processes behind the round-robin proxy (e2e/round-robin-proxy.ts), each
// named `*.multiprocess.e2e.ts`. The default run ignores them, since they need both processes and
// the proxy's own routes; two-process mode (DISPATCH_E2E_PROCESSES=2, `bun run e2e:multiprocess`)
// runs them alone, in Chromium, since every other spec runs against one server already.
const multiprocessSpecs = /\.multiprocess\.e2e\.ts$/;
// The one spec of DISPATCH_E2E_SIGNIN=oidc (e2e/harness-signin.ts), whose servers sign people in
// through the dev OIDC issuer alone: it runs only then, and every other multiprocess spec, which
// signs in through dev sign-in, runs only without it.
const signInSpecs = /\/signin\.multiprocess\.e2e\.ts$/;
if (harnessProcessCount === 2 && process.env.PLAYWRIGHT_BASE_URL) {
  throw new Error(
    "DISPATCH_E2E_PROCESSES=2 runs against two Dispatch servers of this run's own behind the " +
      "round-robin proxy, so it cannot run against PLAYWRIGHT_BASE_URL. Unset one of them."
  );
}

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

// A listing (`--list`) starts no server and runs no spec, so it needs no harness: it is read from
// argv before a `--` separator, and yields to a UI token, since `--ui`/`--ui-*` outrank the
// computed list mode (`lib/cli/testActions.js:52`, `:62`) and a UI session runs specs. The test is
// loose (any `--list`, whatever precedes it), and Playwright reads the `--list` of `--grep --list`
// or `--grep-invert --list` as the option's value, which is a real run. So a listing is exempt only
// from the unset-ports refusal: with every port unset nothing is targeted, and such a run fails at
// e2e/preflight.ts; with any port set it meets the reuse refusal below like any run.
function isListMode(argv: readonly string[]): boolean {
  if (argv.some((arg) => arg === "--ui" || arg.startsWith("--ui-"))) return false;
  const separator = argv.indexOf("--");
  const scanned = separator === -1 ? argv : argv.slice(0, separator);
  return scanned.includes("--list");
}

// This config starts no server. e2e/run.ts starts the harness (e2e/harness-start.ts) on ports
// the OS chooses, exports the ports the servers report, and only then runs Playwright, so this
// module, every worker that re-imports it, and every helper that reads a port at import see the
// real ports when they evaluate. Without it, the run targets servers a person started: every
// port set and DISPATCH_E2E_REUSE_SERVERS=1, since e2e/seed.ts truncates the database those
// servers hold. Anything else is refused here, before a single spec runs, but a listing with every
// port unset; `harnessPorts()` also refuses a malformed or duplicated port, a listing's included.
const runThroughWrapper =
  "bun e2e/run.ts -- bunx playwright test --config e2e/playwright.config.ts [args] (`bun run " +
  "e2e` builds the SPA and runs the same)";
const ports = harnessPorts();
const reuseServers = resolveReuseServers();
const unset = targeted.filter((listener) => ports[listener.key].port === 0);
const listing = isListMode(process.argv);
if (!listing && unset.length > 0) {
  throw new Error(
    `The Dispatch e2e harness is not running: ${unset.map((listener) => harnessVariables[listener.key]).join(", ")} ` +
      `${unset.length > 1 ? "are" : "is"} unset. This config starts no server; run it through ` +
      `e2e/run.ts, which starts the harness on ports the OS chooses and exports them: ` +
      `${runThroughWrapper}. To run against servers you started yourself, set ` +
      `${targeted.map((listener) => harnessVariables[listener.key]).join(", ")} and ` +
      "DISPATCH_E2E_REUSE_SERVERS=1."
  );
}
if (!reuseServers && (!listing || unset.length < targeted.length)) {
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

// Two-process mode's one project: the multiprocess specs, in Chromium, the sign-in spec alone under
// DISPATCH_E2E_SIGNIN=oidc and every other one without it.
const multiprocessProjects = [
  {
    name: "chromium",
    testMatch: harnessSignIn === "oidc" ? signInSpecs : multiprocessSpecs,
    testIgnore: harnessSignIn === "oidc" ? undefined : signInSpecs,
    use: { ...devices["Desktop Chrome"] },
  },
];

export default defineConfig({
  testDir: ".",
  testMatch: /.*\.e2e\.ts/,
  // The projects below that name no `testIgnore` of their own inherit this one.
  testIgnore: harnessProcessCount === 1 ? multiprocessSpecs : undefined,
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
  // does not read this run's fake Envoy, and in two-process mode a pair whose processes do not
  // both read it or do not accept each other's cookies (e2e/preflight.ts).
  globalSetup: fileURLToPath(new URL("./preflight.ts", import.meta.url)),
  projects:
    harnessProcessCount === 2
      ? multiprocessProjects
      : [
          {
            name: "chromium",
            testIgnore: [plainHttpSpecs, multiprocessSpecs],
            use: { ...devices["Desktop Chrome"] },
          },
          {
            name: "iphone",
            testIgnore: [plainHttpSpecs, multiprocessSpecs],
            grep: phoneTagged,
            use: { ...devices["iPhone 13"], browserName: "chromium" },
          },
          // A caret beside a collaborator's cursor behaves per engine, the issue picker's keyboard-step
          // rule and the header session pickers' (the live view's model and thinking pickers) rest on
          // each engine dispatching a closed select's `change` in the key's own task, deep links meet
          // each engine's chunk cancellation and the margin hold's frame and scroll order,
          // IndexedDB-backed document recovery must run in the engines people use, and a body's Mermaid
          // diagram draws through each engine's SVG parser, shadow DOM and the ELK layout running in it.
          // So those specs also run in WebKit. So does the one-socket invariant (`live-socket`): a tab
          // holds one live socket in every engine, and WebKit is the one iOS Safari uses for every
          // subscription its row opens. The keyed write path (`keyed-writes`, `keyed-send`) runs here and
          // in Firefox too: each engine has its own way of failing a request, aborting one, keeping a
          // tab's storage across a reload and asking before it unloads, and a write must land once
          // through all of them.
          {
            name: "webkit",
            testMatch:
              /(collab-cursor|deep-links|keyboard-agents-picker|keyboard-session-pickers|keyed-send|keyed-writes|live-socket|mermaid|offline-edits)\.e2e\.ts/,
            use: { ...devices["Desktop Safari"] },
          },
          // The live view's phone layout (its keyboard cap, gutter and scroll locks), its model
          // pill's ellipsis (the two engines lay out a flex child's min-width differently), the
          // header's model and thinking pickers on a phone, what the Conversation's floating pills
          // cover on a phone, and the one-socket invariant also run in WebKit, the engine iOS Safari
          // uses; only those rows, since the rest of each spec is engine-agnostic.
          {
            name: "webkit-iphone",
            testMatch:
              /(agent-view|agent-view-settings|phone-conversation|delivery-live|live-socket)\.e2e\.ts/,
            grep: /on a phone the live view|a document-scrolling route|covers none of|a capped model name|on a phone the delivery page|the header shows the session's reported model|a tab holds one live socket for every live view/,
            use: { ...devices["iPhone 13"] },
          },
          // Firefox's native editing mishandles text typed over what follows a block's last line break,
          // the issue picker's and the header session pickers' keyboard-step rule rests on the engine's
          // select dispatch, offline document recovery owns browser storage, a click on a link
          // inside the editor meets the engine's focus rule (Firefox focuses the editing host, not the
          // link), and a body's Mermaid diagram draws through the engine's SVG parser, shadow DOM and
          // the ELK layout running in it. Those specs, the whole deep-links spec, the one-socket
          // invariant (`live-socket`, which holds in every engine) and the keyed write path run in
          // Firefox too.
          {
            name: "firefox",
            testMatch:
              /(code-line-replace|deep-links|keyboard-agents-picker|keyboard-session-pickers|keyed-send|keyed-writes|live-socket|mermaid|offline-edits|picture-link-click)\.e2e\.ts/,
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
          // The keyed write path's sign-out rows on that same plain-HTTP origin, which is no secure
          // context, so the browser has no `navigator.locks`: there a tab holds the person's writes for
          // another tab's sign-out on the lease that tab renews (`SIGN_OUT_RENEW_MS`,
          // `SIGN_OUT_LEASE_MS`), which a secure context never reaches.
          {
            name: "chromium-plain-http-sign-out",
            testMatch: /keyed-writes\.e2e\.ts/,
            grep: /a sign-out/,
            use: {
              ...devices["Desktop Chrome"],
              baseURL: plainHttpOrigin,
              launchOptions: { args: [`--host-resolver-rules=MAP ${plainHttpHost} 127.0.0.1`] },
            },
          },
        ],
});
