// The docs media tooling's shared harness: it boots Dispatch the way the e2e suite does
// (`packages/dispatch/e2e/playwright.config.ts`: the fake Envoy, the fake GitHub, the fake secrets
// broker, and `run-server.sh`, which signs a browser in at its dev sign-in route) on ports of its
// own, seeds example data, opens a signed-in browser context at a docs viewport, and checks a page
// is ready and clean before it is captured. `shots.ts` and `walkthrough.ts` both run on it, as does
// any section's own media.
import { type ChildProcess, spawn } from "node:child_process";
import { createWriteStream, existsSync, mkdirSync, readFileSync } from "node:fs";
import { connect } from "node:net";
import { join, resolve } from "node:path";

import {
  type Browser,
  type BrowserContext,
  expect as baseExpect,
  devices,
  type Page,
} from "@playwright/test";

import type { SeededWorkspace } from "../../../packages/dispatch/e2e/workspace";

export const REPO = resolve(import.meta.dir, "../../..");
const E2E = join(REPO, "packages/dispatch/e2e");
const WEB_DIST = join(REPO, "packages/dispatch/web/dist/index.html");
/** Scratch output (server logs, raw recordings, narration cache); gitignored. */
export const WORK = join(import.meta.dir, ".work");

// The e2e suite's own port variables, because `run-server.sh` and `e2e/harness-ports.ts` read
// them. The defaults differ from the suite's (8777, 9021, 9022, 9024) so a docs run and an e2e run
// can share a machine; each still needs its own DATABASE_URL.
const PORTS = {
  DISPATCH_E2E_PORT: process.env.DISPATCH_E2E_PORT || "8786",
  FAKE_BROKER_PORT: process.env.FAKE_BROKER_PORT || "9088",
  FAKE_ENVOY_PORT: process.env.FAKE_ENVOY_PORT || "9086",
  FAKE_GITHUB_PORT: process.env.FAKE_GITHUB_PORT || "9087",
};
const BOOT_TIMEOUT_MS = 600_000;
/** The signed-in human every capture shows. `run-server.sh` allows alice and bob. */
export const VIEWER = "alice";
/** Playwright's assertions with the e2e suite's 15 s wait (`e2e/playwright.config.ts`). */
export const expect = baseExpect.configure({ timeout: 15_000 });

export interface Harness {
  readonly baseURL: string;
  /** Truncates every table, empties the fake Envoy, and revokes every session cookie. */
  reset(): Promise<void>;
}

function portTaken(port: number): Promise<boolean> {
  const dial = (host: string) => {
    const { promise, resolve } = Promise.withResolvers<boolean>();
    const socket = connect(port, host)
      .on("error", () => resolve(false))
      .on("connect", () => {
        socket.end();
        resolve(true);
      });
    return promise;
  };
  return Promise.all([dial("127.0.0.1"), dial("::1")]).then((used) => used.some(Boolean));
}

interface Started {
  readonly child: ChildProcess;
  readonly log: string;
  readonly name: string;
}

function start(name: string, command: string, args: string[]): Started {
  mkdirSync(join(WORK, "harness"), { recursive: true });
  const log = join(WORK, "harness", `${name}.log`);
  const out = createWriteStream(log);
  // A process group of its own, so stopping it reaches `go run`'s compiled child too.
  const child = spawn(command, args, {
    cwd: REPO,
    detached: true,
    env: { ...process.env, ...PORTS },
    stdio: ["ignore", "pipe", "pipe"],
  });
  child.stdout?.pipe(out);
  child.stderr?.pipe(out);
  return { child, log, name };
}

async function ready(url: string, started: Started): Promise<void> {
  const deadline = Date.now() + BOOT_TIMEOUT_MS;
  for (;;) {
    if (started.child.exitCode !== null) {
      throw new Error(`${started.name} exited with ${started.child.exitCode}; see ${started.log}`);
    }
    try {
      await fetch(url);
      return;
    } catch {
      // not listening yet
    }
    if (Date.now() > deadline) {
      throw new Error(`${started.name} was not ready within 600 s; see ${started.log}`);
    }
    await Bun.sleep(250);
  }
}

async function stop(child: ChildProcess): Promise<void> {
  const pid = child.pid;
  if (pid === undefined || child.exitCode !== null || child.signalCode !== null) return;
  const exited = Promise.withResolvers<void>();
  child.once("exit", () => exited.resolve());
  process.kill(-pid, "SIGTERM");
  const kill = setTimeout(() => process.kill(-pid, "SIGKILL"), 10_000);
  await exited.promise;
  clearTimeout(kill);
}

/** Starts the harness, runs `body`, and stops every process it started. */
export async function withHarness<T>(body: (harness: Harness) => Promise<T>): Promise<T> {
  if (!process.env.DATABASE_URL) {
    throw new Error("DATABASE_URL must name a Postgres database this run may truncate and reseed.");
  }
  if (!existsSync(WEB_DIST)) {
    throw new Error(
      `${WEB_DIST} is missing: build the dashboard first (cd packages/dispatch && bun run build:web).`
    );
  }
  const taken: string[] = [];
  for (const [variable, port] of Object.entries(PORTS)) {
    if (await portTaken(Number(port))) taken.push(`${port} (${variable})`);
  }
  if (taken.length > 0) {
    throw new Error(
      `The docs harness cannot start: ${taken.join(", ")} already in use. Stop whatever listens ` +
        "there, or set those variables to free ports."
    );
  }
  // `e2e/harness-ports.ts` reads these when the seeding modules are first imported. The docs show
  // the credential feature on, against the fake broker this harness starts, whatever the shell's
  // broker switch (`packages/dispatch/e2e/harness-broker.ts`) says; unset is that fake.
  Object.assign(process.env, PORTS);
  delete process.env.DISPATCH_E2E_AGENT_SECRETS_URL;
  const baseURL = `http://127.0.0.1:${PORTS.DISPATCH_E2E_PORT}`;

  const processes = [
    start("fake-envoy", "bun", [join(E2E, "fake-envoy.ts")]),
    start("fake-github", "bun", [join(E2E, "fake-github.ts")]),
    start("fake-broker", "bun", [join(E2E, "fake-broker.ts")]),
    start("dispatch", "bash", [join(E2E, "run-server.sh")]),
  ];
  const stopAll = () => Promise.all(processes.map((entry) => stop(entry.child)));
  const onSignal = () => void stopAll().then(() => process.exit(130));
  process.once("SIGINT", onSignal);
  process.once("SIGTERM", onSignal);
  try {
    const [envoy, github, broker, dispatch] = processes;
    await ready(`http://127.0.0.1:${PORTS.FAKE_ENVOY_PORT}/`, envoy);
    await ready(`http://127.0.0.1:${PORTS.FAKE_GITHUB_PORT}/`, github);
    await ready(`http://127.0.0.1:${PORTS.FAKE_BROKER_PORT}/`, broker);
    await ready(`${baseURL}/healthz`, dispatch);
    const { resetDatabase } = await import("../../../packages/dispatch/e2e/seed");
    return await body({
      baseURL,
      reset: resetDatabase,
    });
  } catch (error) {
    for (const entry of processes) {
      if (entry.child.exitCode !== null && entry.child.exitCode !== 0) {
        const tail = readFileSync(entry.log, "utf8").split("\n").slice(-20).join("\n");
        console.error(`--- ${entry.name} exited ${entry.child.exitCode}; last lines:\n${tail}`);
      }
    }
    throw error;
  } finally {
    process.off("SIGINT", onSignal);
    process.off("SIGTERM", onSignal);
    await stopAll();
  }
}

// The e2e suite's modules are imported when called, never at the top of a module:
// `e2e/harness-ports.ts` reads the port variables when it is first evaluated, and `withHarness`
// sets them only once it knows the ports are free. A seed reaches the suite's helpers through
// these two.

/** The e2e suite's API client (`packages/dispatch/e2e/api.ts`): alice by her session cookie, or
 *  an agent by the harness's bearer. */
export const dispatchApi = () => import("../../../packages/dispatch/e2e/api");
/** The fake Envoy's fixtures (`packages/dispatch/e2e/agents.ts`): which sessions are live. */
export const fakeEnvoy = () => import("../../../packages/dispatch/e2e/agents");

export interface DispatchWorkspace extends SeededWorkspace {
  /** The Reviewer's comment anchored on the workflow spec's "seeded corpus", and alice's reply. */
  readonly marginThread: { readonly commentId: string; readonly markId: string };
}

/** The e2e suite's realistic workspace (`seedWorkspace`), plus the one thing the docs show that it
 *  leaves out: a comment thread anchored on a spec. Call `reset()` first. */
export async function seedDispatchWorkspace(): Promise<DispatchWorkspace> {
  const { seedWorkspace } = await import("../../../packages/dispatch/e2e/workspace");
  const { createComment } = await dispatchApi();
  const seeded = await seedWorkspace();
  const comment = await createComment(
    seeded.issues.workflow,
    {
      anchor: { artifact: "spec", quote: "seeded corpus" },
      body: "Does the seeded corpus cover the snoozed and the retracted asks?",
    },
    {
      actor: { id: "reviewer-session", kind: "session", origin: { session_title: "Reviewer" } },
      as: "agent",
    }
  );
  if (comment.anchor === null) throw new Error("seedDispatchWorkspace: the comment has no anchor");
  await createComment(seeded.issues.workflow, {
    body: "Yes: one of each, plus an ask on the Runbook document.",
    reply_to: comment.id,
  });
  return { ...seeded, marginThread: { commentId: comment.id, markId: comment.anchor.mark_id } };
}

export type Viewport = "desktop" | "phone";
export type Theme = "light" | "dark";

export interface ViewerOptions {
  readonly viewport: Viewport;
  readonly theme: Theme;
  /** Overrides the desktop size (CSS pixels); a phone is always the iPhone 13 profile. */
  readonly size?: { readonly width: number; readonly height: number };
  readonly deviceScaleFactor?: number;
  readonly recordVideo?: {
    readonly dir: string;
    readonly size: { readonly width: number; readonly height: number };
  };
}

const DESKTOP = { width: 1440, height: 900 };

/** A browser context signed in as `VIEWER` with the session cookie the server's dev sign-in route
 *  mints, the one the e2e API client (`userHeaders`) signs in with. Playwright's own request client
 *  (`e2e/users.ts`'s `signIn`) is not used: under Bun 1.3 its Set-Cookie parser is handed the
 *  route's path as the response URL, throws, and the sign-in never settles. The cookie names a
 *  session the next `reset()` revokes, so open a context after the reset. */
export async function newViewerContext(
  browser: Browser,
  baseURL: string,
  options: ViewerOptions
): Promise<BrowserContext> {
  const { sessionCookieName, userHeaders } = await dispatchApi();
  const cookie = (await userHeaders(VIEWER)).Cookie;
  if (!cookie.startsWith(`${sessionCookieName}=`)) {
    throw new Error(`the dev sign-in answered an unexpected cookie: ${cookie.split("=")[0]}`);
  }
  const { defaultBrowserType: _engine, ...iphone } = devices["iPhone 13"];
  const device =
    options.viewport === "phone"
      ? iphone
      : { viewport: options.size ?? DESKTOP, deviceScaleFactor: 2 };
  const context = await browser.newContext({
    ...device,
    ...(options.deviceScaleFactor === undefined
      ? {}
      : { deviceScaleFactor: options.deviceScaleFactor }),
    baseURL,
    colorScheme: options.theme,
    reducedMotion: "reduce",
    ...(options.recordVideo === undefined ? {} : { recordVideo: options.recordVideo }),
  });
  await context.addCookies([
    { name: sessionCookieName, url: baseURL, value: cookie.slice(sessionCookieName.length + 1) },
  ]);
  return context;
}

/** Collects every uncaught exception the page throws; a capture with one is refused. */
export function pageErrors(page: Page): string[] {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  return errors;
}

/** Waits until nothing on screen is loading: no visible `aria-busy` skeleton, every image
 *  decoded, the web fonts in, and two frames painted after that. */
export async function waitForSettled(page: Page): Promise<void> {
  await page.waitForFunction(() =>
    [...document.querySelectorAll('[aria-busy="true"]')].every(
      (element) => element.getClientRects().length === 0
    )
  );
  await page.waitForFunction(() => [...document.images].every((image) => image.complete));
  await page.evaluate(async () => {
    await document.fonts.ready;
    const painted = Promise.withResolvers<void>();
    requestAnimationFrame(() => requestAnimationFrame(() => painted.resolve()));
    await painted.promise;
  });
}

/**
 * Refuses a screen a reader should never see in the docs: the not-found page, a visible error
 * (`role="alert"`, which every Dispatch error state carries, the error boundary included), an
 * empty state (`EmptyState` renders a labelled dashed `section`) other than one the capture names
 * in `allowEmpty`, or an uncaught page exception.
 */
export async function assertScreenClean(
  page: Page,
  errors: readonly string[],
  allowEmpty: readonly string[] = []
): Promise<void> {
  const problems = errors.map((message) => `page error: ${message}`);
  if ((await page.title()).startsWith("Not found"))
    problems.push(`not-found page at ${page.url()}`);
  for (const alert of await page.locator('[role="alert"]:visible').all()) {
    problems.push(`error on screen: ${(await alert.innerText()).trim() || "(no text)"}`);
  }
  for (const empty of await page.locator("section.border-dashed[aria-label]:visible").all()) {
    const label = (await empty.getAttribute("aria-label")) ?? "";
    if (!allowEmpty.includes(label)) problems.push(`empty state on screen: ${label}`);
  }
  if (problems.length > 0) throw new Error(problems.join("; "));
}
