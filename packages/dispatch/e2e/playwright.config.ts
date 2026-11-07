import { fileURLToPath } from "node:url";
import { defineConfig, devices, type Project } from "@playwright/test";
import { baseUrl } from "./api";
import { targetedListeners } from "./harness-listeners";
import {
  harnessPorts,
  harnessVariables,
  laneCount,
  laneCountVariable,
  workerCount,
  workerCountVariable,
} from "./harness-ports";
import { harnessProcessCount } from "./harness-processes";
import { harnessSignIn } from "./harness-signin";
import { measuring } from "./measure";
import { plainHttpHost, plainHttpOrigin } from "./plain-http-origin";

// The plain-HTTP project's origin is a proxy on PLAIN_HTTP_PORT, reached by a host name Chromium
// maps to loopback. The page is a non-loopback plain-HTTP origin, while the proxy forwards to the
// dev sign-in server under its own loopback origin. `plainHttpSpecs` are the files that project
// runs, the origin's own rows and the proxy's; `chromium` and `iphone` ignore them, since they
// open the plain-HTTP name, which only that project maps, and the origin rows' first assertion
// fails on loopback by design.
const plainHttpSpecs = /plain-http-(origin|proxy)\.e2e\.ts/;
// The `iphone`, `webkit` and `firefox` projects run only the rows tagged `@phone`, `@webkit` and
// `@firefox`, written at the end of the test's title. Which tests take `@phone` is in
// `packages/dispatch/AGENTS.md`, "Phone acceptance"; each engine tag names a row that exercises
// one engine behaviour the comment above those projects lists, and a comment above the row names
// which; a row that only repeats an assertion Chromium already made carries no tag. An untagged
// test drops out with no failure. The pattern is anchored so a tag matches only as a whole
// whitespace-separated token, never inside a longer one such as `@phones`, `x@phone`,
// `@webkit-iphone` or `@firefox-alone`.
function tagged(tag: string): RegExp {
  return new RegExp(`(^|\\s)@${tag}(\\s|$)`);
}
const phoneTagged = tagged("phone");
const webkitTagged = tagged("webkit");
const firefoxTagged = tagged("firefox");
// A row tagged `@alone` is a measurement: what it reads - how many bytes of JavaScript a page
// made the browser fetch - is read against the machine rather than against the page alone, and
// four lanes on one machine change what a page gets through before the row looks. `chromium`
// takes every such row out of its own run (`grepInvert`) and `chromium-alone-rows` at the end of
// this file runs them, one at a time, with nothing else on the machine. The tag, rather than the
// row's title, is what the two projects agree on, so a row renamed keeps its project and a row
// that drops the tag goes back to the four-lane run rather than nowhere.
const aloneTagged = tagged("alone");
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

// How many workers the command line asks for, if it does: `--workers <n>`, `--workers=<n>`,
// `-j <n>` or `-j<n>`, read before a `--` separator as the list mode above is. A worker is a
// lane, and e2e/run.ts started one harness per lane before Playwright, so a command line that
// asks for another number is refused rather than quietly leaving a lane unused or putting two
// workers on one lane's servers.
function workersOnCommandLine(argv: readonly string[]): string | undefined {
  const separator = argv.indexOf("--");
  const scanned = separator === -1 ? argv : argv.slice(0, separator);
  for (const [index, argument] of scanned.entries()) {
    if (argument === "--workers" || argument === "-j") return scanned[index + 1];
    if (argument.startsWith("--workers=")) return argument.slice("--workers=".length);
    if (argument.startsWith("-j") && argument.length > 2) return argument.slice(2);
  }
  return undefined;
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

// One worker per lane, and the lanes are e2e/run.ts's: a worker resolves its own harness from
// `TEST_PARALLEL_INDEX` (e2e/harness-ports.ts), so a run with more workers than lanes would put
// two workers on one lane's servers and database, which e2e/seed.ts truncates per row.
const lanes = laneCount();
const asked = workersOnCommandLine(process.argv);
if (asked !== undefined && asked !== String(lanes)) {
  throw new Error(
    `This run has ${lanes} lane${lanes === 1 ? "" : "s"}, each a harness of its own, so ` +
      `--workers ${asked} would not match: a worker reads the lane of its own index. Set ` +
      `${workerCountVariable}=${asked} instead, which starts that many lanes and runs that many ` +
      "workers."
  );
}

// `laneCount` answers the worker count where `DISPATCH_E2E_LANES` is unset, so that a run against
// servers a person started reads its own. That fallback is right for one worker and wrong for
// more: nothing exported a lane, so every worker would resolve lane 0 and share one harness's
// servers and database, which e2e/seed.ts truncates per row - rows would delete each other's
// fixtures and fail in ways that read as product faults. Only e2e/run.ts exports that variable.
if (process.env[laneCountVariable] === undefined && workerCount() > 1) {
  throw new Error(
    `${workerCountVariable}=${workerCount()} asks for more than one worker, but nothing exported ` +
      `${laneCountVariable}, so there is one harness and every worker would share it. Run this ` +
      `config through e2e/run.ts, which starts a lane per worker and exports that count: ` +
      `${runThroughWrapper}.`
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

// The specs whose rows are read from the wall clock: that a document write answers in under a
// second (`heavy-document-writes.e2e.ts`), and that a card's draw holds the main thread for less
// than one (`heavy-bodies.e2e.ts` and `nested-directive.e2e.ts`, both through
// `main-thread-hold.ts`). Four lanes on one machine leave such a claim unmeasurable - the wall
// time of a write holds what the other three lanes took as surely as the work the server did -
// so these files run in the `-alone` projects at the end of this list, after every other project
// and one row at a time, and in no project above them (sjawhar/legion#2612).
const clockSpecs = /(heavy-bodies|heavy-document-writes|nested-directive)\.e2e\.ts/;

// Every project that runs four lanes wide. The `-alone` projects below name all of them, so a
// project added here is one more the measured rows wait for.
const parallelProjects = [
  // Every row of every spec, less the measurements: a row tagged `@alone` belongs to
  // `chromium-alone-rows` below, and running it here as well would both measure it under three
  // other lanes' load and run it twice.
  {
    name: "chromium",
    grepInvert: aloneTagged,
    testIgnore: [plainHttpSpecs, multiprocessSpecs, clockSpecs],
    use: { ...devices["Desktop Chrome"] },
  },
  {
    name: "iphone",
    testIgnore: [plainHttpSpecs, multiprocessSpecs, clockSpecs],
    grep: phoneTagged,
    use: { ...devices["iPhone 13"], browserName: "chromium" as const },
  },
  // WebKit runs the rows a WebKit decision can change, and nothing else: it selects by the
  // `@webkit` tag, so each row it runs carries that tag and a comment naming the clause below it
  // exercises. Every clause here names one engine behaviour and the rows that exercise it.
  // Where a caret lands beside a collaborator's own caret: `collab-cursor`'s three rows.
  // A closed select's `change` dispatched inside the key's own task, which the Agents issue
  // picker's and the live header pickers' keyboard-step rule rests on: the six rows of
  // `keyboard-agents-picker` that step a select from the keyboard or leave one without Enter,
  // and all five of `keyboard-session-pickers`.
  // The engine's cancellation of a chunk the navigation away from it left downloading, and the
  // margin hold's frame and scroll order: six `deep-links` rows - the held chunk's, the press
  // raced against the landing's own frames, the two hidden-editor rows that run with the page's
  // animation frames held, the per-frame scroll of a past version's landing, and the placement
  // the margin's own scroll must not move.
  // IndexedDB-backed recovery of an edit the server has not taken: three `offline-edits` rows -
  // the edit restored across a reload, the acknowledged one cleared, and two tabs holding two
  // connections to one store.
  // The SVG parser, the shadow DOM and the ELK layout a diagram draws through: `mermaid`'s one
  // row.
  // A Web Worker's chunk, its deadline and a paint that arrives after it: `heavy-streamed-turn`'s
  // row, where the body grows under its own paint as a turn streams. The body a reader opens is
  // `heavy-bodies`, which runs in `webkit-alone` below, since its rows are read from the clock.
  // The one-socket invariant: `live-socket`'s row that a tab holds one live socket for every live
  // view, since WebKit is the engine iOS Safari opens every subscription in.
  // The keyed write path's request failure, its abort, a tab's storage across a reload and the
  // ask before a tab unloads: five `keyed-writes` rows and two `keyed-send` rows.
  // A sandboxed frame's own origin, what a page inside it may reach, the `postMessage` pair that
  // carries its data and the height it reports: every row of `view-block`, `view-sandbox` and
  // `view-grants`, since WebKit is the engine iOS Safari gives a `view` block's page. The block's
  // byte budget (`view-budget`) is Chromium's alone; that file says why.
  // Without a `testMatch` the project would see every spec in this directory, so it names the
  // same `testIgnore` as `chromium`: no tagged row may sit in a plain-HTTP, multiprocess or
  // clock-read spec, and `heavy-bodies.e2e.ts` belongs to `webkit-alone` alone.
  {
    name: "webkit",
    testIgnore: [plainHttpSpecs, multiprocessSpecs, clockSpecs],
    grep: webkitTagged,
    use: { ...devices["Desktop Safari"] },
  },
  // The live view's phone layout (its keyboard cap, gutter and scroll locks), its model
  // pill's ellipsis (the two engines lay out a flex child's min-width differently), the
  // header's model and thinking pickers on a phone, what the Conversation's floating pills
  // cover on a phone, and the one-socket invariant also run in WebKit, the engine iOS Safari
  // uses; only those rows, since the rest of each spec is engine-agnostic.
  // This project keeps its file `testMatch` and its title `grep` rather than an engine tag, and
  // Playwright reads an array of regexes as OR (`createTitleMatcher` answers on the first that
  // matches), so a tag beside the titles below would widen the project rather than narrow it.
  // The `view` block's three engine specs join it by file and by the phrase `a view block`, which
  // every one of their titles holds: a frame's sandbox, its opaque origin, what a page in it may
  // reach, the `postMessage` pair and the frame's height are each engine's own, and this project
  // is where they meet WebKit on a phone. A file matched here with no title in the `grep` runs
  // zero tests and looks green, so a row added to one of those files keeps that phrase.
  {
    name: "webkit-iphone",
    testMatch:
      /(agent-view|agent-view-settings|phone-conversation|delivery-live|live-socket|view-block|view-sandbox|view-grants)\.e2e\.ts/,
    grep: /on a phone the live view|a document-scrolling route|covers none of|a capped model name|on a phone the delivery page|the header shows the session's reported model|a tab holds one live socket for every live view|a view block/,
    use: { ...devices["iPhone 13"] },
  },
  // Firefox runs the rows a Firefox decision can change, and nothing else: it selects by the
  // `@firefox` tag, so each row it runs carries that tag and a comment naming the clause below it
  // exercises. Every clause here names one engine behaviour and the rows that exercise it.
  // Native editing over what follows a block's last line break, which this engine mishandles:
  // `code-line-replace`'s two rows.
  // The engine's focus rule for a click on a link inside the editor, where Firefox focuses the
  // editing host rather than the link: `picture-link-click`'s one row.
  // A closed select's `change` dispatched inside the key's own task, which the Agents issue
  // picker's and the live header pickers' keyboard-step rule rests on: the six rows of
  // `keyboard-agents-picker` that step a select from the keyboard or leave one without Enter,
  // and all five of `keyboard-session-pickers`.
  // The engine's cancellation of a chunk the navigation away from it left downloading, and the
  // margin hold's frame and scroll order: six `deep-links` rows - the held chunk's, the press
  // raced against the landing's own frames, the two hidden-editor rows that run with the page's
  // animation frames held, the per-frame scroll of a past version's landing, and the placement
  // the margin's own scroll must not move.
  // IndexedDB-backed recovery of an edit the server has not taken: three `offline-edits` rows -
  // the edit restored across a reload, the acknowledged one cleared, and two tabs holding two
  // connections to one store.
  // The SVG parser, the shadow DOM and the ELK layout a diagram draws through: `mermaid`'s one
  // row.
  // A Web Worker's chunk, its deadline and a paint that arrives after it: `heavy-streamed-turn`'s
  // row, where the body grows under its own paint as a turn streams. The body a reader opens is
  // `heavy-bodies`, which runs in `firefox-alone` below, since its rows are read from the clock.
  // The one-socket invariant: `live-socket`'s row that a tab holds one live socket for every live
  // view, which must hold in every engine people read in.
  // The keyed write path's request failure, its abort, a tab's storage across a reload and the
  // ask before a tab unloads: five `keyed-writes` rows and two `keyed-send` rows.
  // A sandboxed frame's own origin, what a page inside it may reach, the `postMessage` pair that
  // carries its data and the height it reports: every row of `view-block`, `view-sandbox` and
  // `view-grants`, for the reason given above `webkit`. The block's byte budget (`view-budget`)
  // is Chromium's alone; that file says why.
  // The `testIgnore` is `chromium`'s, for the reason given above `webkit`.
  {
    name: "firefox",
    testIgnore: [plainHttpSpecs, multiprocessSpecs, clockSpecs],
    grep: firefoxTagged,
    use: { ...devices["Desktop Firefox"] },
  },
  // The one row of `shell.e2e.ts` that follows a Download link before a chunk fails. It keeps its
  // file `testMatch` and its title `grep` rather than an engine tag, for the reason given above
  // `webkit-iphone`: one row is as narrow as a tag would make it, and a tag added beside the
  // title below would be read as OR and widen the project.
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
] satisfies Project[];

/**
 * The last projects of a run: the rows whose claim is a measurement - a wall-clock time, or how
 * many bytes of JavaScript a page made the browser fetch - with the machine to themselves.
 *
 * Each names in `dependencies` every project before it, so Playwright starts the first of them
 * only once every parallel project has finished, and runs these four one after another rather
 * than four at a time. Ordering alone is not quiet: a project's own rows would still run four
 * at a time, so each caps its own workers at one. Playwright 1.64.1 takes that cap per project -
 * `testProject.workers`, "The maximum number of concurrent worker processes to use for
 * parallelizing tests from this project", which its dispatcher holds a project's running jobs to
 * (`_workerLimitPerProjectId`, `playwright/lib/runner/index.js`) - and the run's own `workers`
 * above stays at the lane count, so the suite ahead of these projects runs as wide as ever. A
 * worker here resolves a lane as any other worker does (`TEST_PARALLEL_INDEX`,
 * e2e/harness-ports.ts), so these rows run against one lane's harness while the other lanes
 * stand idle.
 *
 * A project whose dependency has a failing row is not run at all: in a red run these rows are
 * left unrun rather than measured beside whatever the failing row's retry is doing.
 *
 * A command line that names one of these files runs the whole suite, since a project's
 * dependencies are pulled in whole: a file filter narrows a project it selects, never a project
 * selected to run before it. `--no-deps` is how one of these files is run by itself, and such a
 * run is as quiet as the machine it is on.
 *
 * Each engine keeps the rows it ran. `heavy-bodies.e2e.ts` ran in `chromium`, `firefox` and
 * `webkit`, for the reasons written above those projects, and runs here in all three;
 * `heavy-document-writes.e2e.ts`, which drives no browser, and `nested-directive.e2e.ts` ran in
 * `chromium` alone and run in `chromium-alone` alone.
 *
 * The first three select by `testMatch` alone, where `webkit` and `firefox` select by an engine
 * tag: their axis is the file, not a behaviour. Every row of a clock-read file runs here
 * whatever it asserts, because what puts it here is that its claim is a wall-clock time, and
 * `heavy-bodies.e2e.ts` carries no engine tag at all - the two tagged projects ignore it.
 *
 * `chromium-alone-rows` is the one that selects by tag, because its axis is the row: the byte
 * measure of `view-budget.e2e.ts` must be alone, and the five frame rows beside it in that file
 * must not - they read what the frames of one page asked for, which four lanes do not change,
 * and a row left unrun behind a red elsewhere is a row that stops catching anything. It takes
 * whatever `chromium` drops for the tag, so a tagged row can never fall between the two.
 */
const aloneProjects = [
  {
    name: "chromium-alone",
    dependencies: parallelProjects.map((project) => project.name),
    testMatch: clockSpecs,
    use: { ...devices["Desktop Chrome"] },
    workers: 1,
  },
  {
    name: "firefox-alone",
    dependencies: [...parallelProjects.map((project) => project.name), "chromium-alone"],
    testMatch: /heavy-bodies\.e2e\.ts/,
    use: { ...devices["Desktop Firefox"] },
    workers: 1,
  },
  {
    name: "webkit-alone",
    dependencies: [
      ...parallelProjects.map((project) => project.name),
      "chromium-alone",
      "firefox-alone",
    ],
    testMatch: /heavy-bodies\.e2e\.ts/,
    use: { ...devices["Desktop Safari"] },
    workers: 1,
  },
  // The `@alone` rows of every spec `chromium` runs: it ignores the files `chromium` ignores, so
  // it and `chromium-alone` never see one row twice, and it selects the tag `chromium` inverts.
  {
    name: "chromium-alone-rows",
    dependencies: [
      ...parallelProjects.map((project) => project.name),
      "chromium-alone",
      "firefox-alone",
      "webkit-alone",
    ],
    grep: aloneTagged,
    testIgnore: [plainHttpSpecs, multiprocessSpecs, clockSpecs],
    use: { ...devices["Desktop Chrome"] },
    workers: 1,
  },
] satisfies Project[];

export default defineConfig({
  testDir: ".",
  testMatch: /.*\.e2e\.ts/,
  // The projects below that name no `testIgnore` of their own inherit this one.
  testIgnore: harnessProcessCount === 1 ? multiprocessSpecs : undefined,
  outputDir: "test-results",
  // Every row is independent: it empties its lane's database and fake listeners itself
  // (e2e/seed.ts), and a worker never runs two rows at once, so rows of one file may run in
  // different lanes. A file whose rows must run in order says so itself, with
  // `test.describe.configure({ mode: "serial" })` and the reason.
  fullyParallel: true,
  workers: lanes,
  // With E2E_MEASURE_FILE set, the timing fixture's reporter writes one row per test beside the
  // list reporter's own output (e2e/measure-reporter.ts); without it the run is untouched.
  reporter: measuring
    ? [["list"], [fileURLToPath(new URL("./measure-reporter.ts", import.meta.url))]]
    : "list",
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
    harnessProcessCount === 2 ? multiprocessProjects : [...parallelProjects, ...aloneProjects],
});
