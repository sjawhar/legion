// The docs media tooling's shared harness: it boots Dispatch the way the e2e suite does, through
// `packages/dispatch/e2e/harness-start.ts` (the fake Envoy, the fake GitHub, the fake secrets
// broker, `run-server.sh`, which signs a browser in at its dev sign-in route, and the plain-HTTP
// proxy) on ports the OS chooses, seeds example data, opens a signed-in browser context at a docs
// viewport, and checks a page is ready and clean before it is captured. `shots.ts` and
// `walkthrough.ts` both run on it, as does any section's own media.
import { existsSync, readFileSync } from "node:fs";
import { join, relative, resolve } from "node:path";

import {
  type Browser,
  type BrowserContext,
  expect as baseExpect,
  devices,
  type Page,
} from "@playwright/test";

import type {
  Harness as E2eHarness,
  HarnessProcess,
} from "../../../packages/dispatch/e2e/harness-start";
import type { SeededWorkspace } from "../../../packages/dispatch/e2e/workspace";

export const REPO = resolve(import.meta.dir, "../../..");
const WEB_DIST = join(REPO, "packages/dispatch/web/dist/index.html");
/** Scratch output (server logs, raw recordings, narration cache); gitignored. */
export const WORK = join(import.meta.dir, ".work");

/** The signed-in human every capture shows. `run-server.sh` allows alice and bob. */
export const VIEWER = "alice";
/** Playwright's assertions with the e2e suite's 15 s wait (`e2e/playwright.config.ts`). */
export const expect = baseExpect.configure({ timeout: 15_000 });

/** How long one step of a run may take: a set's reset and seed, or one shot from its `prepare` to
 *  its capture. Playwright bounds its own calls, but an API write and a page script wait as long as
 *  the server or the page takes, so a run that stops answering would otherwise hold CI until the
 *  job's own limit. */
export const STEP_TIMEOUT_MS = 120_000;

/** A step that ran past STEP_TIMEOUT_MS. `withHarness` answers it by having the Dispatch server
 *  dump its goroutines into its log, which shows what the server was waiting on, and the run
 *  fails. */
export class HarnessHang extends Error {}

/** Waits for `step`, and throws HarnessHang naming `what()` once it has taken STEP_TIMEOUT_MS. The
 *  step is not cancelled (a request already sent cannot be taken back); the run ends instead. */
export async function withinStepTimeout<T>(what: () => string, step: Promise<T>): Promise<T> {
  const { promise: timedOut, reject } = Promise.withResolvers<never>();
  const timer = setTimeout(
    () => reject(new HarnessHang(`${what()}: no answer in ${STEP_TIMEOUT_MS / 1000} s`)),
    STEP_TIMEOUT_MS
  );
  try {
    return await Promise.race([step, timedOut]);
  } finally {
    clearTimeout(timer);
  }
}

export interface Harness {
  readonly baseURL: string;
  /** Truncates every table, empties the fake Envoy, and revokes every session cookie. */
  reset(): Promise<void>;
}

/** Prints the end of the Dispatch server's log, then has the server write every goroutine's stack
 *  to that log (Go's answer to SIGQUIT, which also ends the process) and names the file. */
async function dumpGoroutines(dispatch: HarnessProcess): Promise<void> {
  if (dispatch.log === undefined) return;
  const tail = readFileSync(dispatch.log, "utf8").split("\n").slice(-40).join("\n");
  console.error(`--- dispatch's last lines when the run stopped:\n${tail}`);
  const { child } = dispatch;
  if (child.exitCode !== null || child.signalCode !== null) return;
  process.kill(child.pid, "SIGQUIT");
  await Promise.race([dispatch.drained, Bun.sleep(10_000)]);
  console.error(`--- dispatch's goroutines at that moment end ${relative(REPO, dispatch.log)}`);
}

/** Starts the harness, runs `body`, and stops every process it started. A port variable the
 *  caller set (`DISPATCH_E2E_PORT`, `FAKE_ENVOY_PORT`, `FAKE_GITHUB_PORT`, `FAKE_BROKER_PORT`,
 *  `PLAIN_HTTP_PORT`) is used as given; unset, the OS chooses, so a docs run and an e2e run share a
 *  machine without a port of their own, each with its own DATABASE_URL. */
export async function withHarness<T>(body: (harness: Harness) => Promise<T>): Promise<T> {
  if (!process.env.DATABASE_URL) {
    throw new Error("DATABASE_URL must name a Postgres database this run may truncate and reseed.");
  }
  if (!existsSync(WEB_DIST)) {
    throw new Error(
      `${WEB_DIST} is missing: build the dashboard first (cd packages/dispatch && bun run build:web).`
    );
  }
  // The docs show the credential feature on, against the fake broker the harness starts, whatever
  // the shell's broker switch (`packages/dispatch/e2e/harness-broker.ts`) says; unset is that fake.
  // The e2e modules read the switch, and the ports, when they are first evaluated, so they are
  // imported only once the environment holds what they must read.
  delete process.env.DISPATCH_E2E_AGENT_SECRETS_URL;
  const { startHarness } = await import("../../../packages/dispatch/e2e/harness-start");
  const interrupted = new AbortController();
  const starting = startHarness({ logDir: join(WORK, "harness"), signal: interrupted.signal });
  // A stop while the harness starts ends what has started; after, it ends the whole harness.
  const onSignal = () => {
    interrupted.abort();
    void starting
      .then(
        (started) => started.stop(),
        () => undefined
      )
      .then(() => process.exit(130));
  };
  process.once("SIGINT", onSignal);
  process.once("SIGTERM", onSignal);
  let started: E2eHarness | undefined;
  try {
    started = await starting;
    Object.assign(process.env, started.env);
    const { resetDatabase } = await import("../../../packages/dispatch/e2e/seed");
    return await body({
      baseURL: `http://127.0.0.1:${started.env.DISPATCH_E2E_PORT}`,
      reset: resetDatabase,
    });
  } catch (error) {
    const processes = started?.processes ?? [];
    const dispatch = processes.find((entry) => entry.listener.id === "dispatch");
    const hung = error instanceof HarnessHang;
    if (hung && dispatch !== undefined) await dumpGoroutines(dispatch);
    for (const entry of processes) {
      if ((hung && entry === dispatch) || entry.log === undefined) continue;
      if (entry.child.exitCode !== null && entry.child.exitCode !== 0) {
        const tail = readFileSync(entry.log, "utf8").split("\n").slice(-20).join("\n");
        console.error(
          `--- ${entry.listener.id} exited ${entry.child.exitCode}; last lines:\n${tail}`
        );
      }
    }
    throw error;
  } finally {
    process.off("SIGINT", onSignal);
    process.off("SIGTERM", onSignal);
    await started?.stop();
  }
}

// The e2e suite's modules are imported when called, never at the top of a module: they read the
// port variables when they are first evaluated, and `withHarness` sets them only once the
// harness's listeners have reported their ports. A seed reaches the suite's helpers through these
// two.

/** The e2e suite's API client (`packages/dispatch/e2e/api.ts`): alice by her session cookie, or
 *  an agent by the harness's bearer. */
export const dispatchApi = () => import("../../../packages/dispatch/e2e/api");
/** The fake Envoy's fixtures (`packages/dispatch/e2e/agents.ts`): which sessions are live. */
export const fakeEnvoy = () => import("../../../packages/dispatch/e2e/agents");

export interface DispatchWorkspace extends SeededWorkspace {
  /** The Reviewer's comment anchored on the workflow spec's "seeded corpus", and alice's reply. */
  readonly marginThread: { readonly commentId: string; readonly markId: string };
}

/** The e2e suite's realistic workspace (`seedWorkspace`), plus what the docs show that it leaves
 *  out: a comment thread anchored on a spec, the delivery measures' fixture and the Pipeline page's
 *  rows beside it (`e2e/delivery-seed.ts`). Call `reset()` first. */
export async function seedDispatchWorkspace(): Promise<DispatchWorkspace> {
  const { seedWorkspace } = await import("../../../packages/dispatch/e2e/workspace");
  const { seedMeasuresFixture, seedPipelineRows } = await import(
    "../../../packages/dispatch/e2e/delivery-seed"
  );
  const { createComment } = await dispatchApi();
  const seeded = await seedWorkspace();
  await seedMeasuresFixture();
  await seedPipelineRows();
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
