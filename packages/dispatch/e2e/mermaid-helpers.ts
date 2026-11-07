// The init-script and worker helpers `e2e/mermaid.e2e.ts`'s rows share: the wrappers that record,
// hold, refuse or fail the page's `Worker`, the two scroll helpers (one turns the browser's scroll
// anchoring off, one reads the offset once it settles), the route that makes the real worker
// chunk hold a diagram's source, the route that holds Mermaid's own chunks back, and the read
// that names which of the two renderers drew a mounted diagram.
import { setTimeout as sleep } from "node:timers/promises";

import { type BrowserContext, expect, type Locator, type Page } from "@playwright/test";

import type { MermaidRendererName } from "../web/src/features/refs/mermaid-render.worker";

// What this file's init scripts keep on the page's window.
declare global {
  interface Window {
    /** What each worker answered for each source it was posted. */
    __mermaidAnswers?: MermaidAnswer[];
    /** Makes the page's workers fail from now on, as `failMermaidWorkersOnCall` describes. */
    __failMermaidWorkers?: () => void;
    /** The `source` of each request posted to a worker. */
    __mermaidSent?: string[];
    /** How many workers the page constructed. */
    __mermaidWorkers?: number;
    /** Delivers what a held worker posted, and lets later posts through. */
    __releaseMermaid?: () => void;
  }
}

/** Wraps the page's `Worker` so the page counts each worker it constructs
 *  (`window.__mermaidWorkers`) and records the `source` of each request posted to one
 *  (`window.__mermaidSent`), passing everything through to the real worker. Mermaid's worker is
 *  the app's only `new Worker` (`features/refs/mermaid-diagrams.ts`). */
export async function recordMermaidWorkers(page: Page): Promise<void> {
  await page.addInitScript(() => {
    window.__mermaidWorkers = 0;
    window.__mermaidSent = [];
    const RealWorker = window.Worker;
    window.Worker = class extends RealWorker {
      constructor(url: string | URL, options?: WorkerOptions) {
        super(url, options);
        window.__mermaidWorkers = (window.__mermaidWorkers ?? 0) + 1;
      }

      postMessage(message: unknown, options?: StructuredSerializeOptions | Transferable[]): void {
        if (typeof message === "object" && message !== null && "source" in message) {
          window.__mermaidSent?.push(String(message.source));
        }
        super.postMessage(message, options as StructuredSerializeOptions);
      }
    };
  });
}

/** What a worker answered for one source: the markup a renderer drew, with the renderer that
 *  drew it, or the reason it was refused. */
export interface MermaidAnswer {
  error: string | null;
  markup: string | null;
  renderer: MermaidRendererName | null;
  source: string;
}

/** Wraps the page's `Worker` so the page keeps what each worker answered for each source it was
 *  posted (`window.__mermaidAnswers`), passing everything through to the real worker. A row that
 *  reads the markup a renderer wrote has no other way to it: the page mounts the sanitized
 *  drawing alone, so what the sanitizer took out is nowhere in the DOM. The worker answers one
 *  job at a time (`worker-queue.ts`), and each answer names the job's own id, so an answer is
 *  paired with the source that was posted under that id rather than with the one beside it. */
export async function recordMermaidAnswers(page: Page): Promise<() => Promise<MermaidAnswer[]>> {
  await page.addInitScript(() => {
    const answers: MermaidAnswer[] = [];
    window.__mermaidAnswers = answers;
    const posted = new Map<number, string>();
    const RealWorker = window.Worker;
    window.Worker = class extends RealWorker {
      constructor(url: string | URL, options?: WorkerOptions) {
        super(url, options);
        super.addEventListener("message", (event: MessageEvent) => {
          const reply: unknown = event.data;
          if (typeof reply !== "object" || reply === null || !("id" in reply)) return;
          const { error, id, markup, renderer } = reply as {
            error?: string | null;
            id: number;
            markup?: string;
            renderer?: MermaidRendererName;
          };
          // A phase reply carries neither, and says nothing about what was drawn.
          if (markup === undefined && error === undefined) return;
          answers.push({
            error: error ?? null,
            markup: markup ?? null,
            renderer: renderer ?? null,
            source: posted.get(id) ?? "",
          });
        });
      }

      postMessage(message: unknown, options?: StructuredSerializeOptions | Transferable[]): void {
        if (
          typeof message === "object" &&
          message !== null &&
          "id" in message &&
          "source" in message
        ) {
          posted.set(Number(message.id), String(message.source));
        }
        super.postMessage(message, options as StructuredSerializeOptions);
      }
    };
  });
  return () => page.evaluate(() => window.__mermaidAnswers ?? []);
}

/** Wraps the page's `Worker` so nothing its workers post reaches the page until
 *  `window.__releaseMermaid()` runs: each `message` event a listener added with
 *  `addEventListener("message", …)` (as `mermaid-diagrams.ts` listens) or `onmessage` would get
 *  is held, then delivered in arrival order on release, and later ones flow straight through.
 *  The worker's `ready` is held too, and the time limit starts only at `ready`, so a held diagram
 *  is never refused; it stays undrawn until the release. */
export async function holdMermaidWorkers(page: Page): Promise<void> {
  await page.addInitScript(() => {
    let released = false;
    const held: (() => void)[] = [];
    window.__releaseMermaid = () => {
      released = true;
      for (const deliver of held.splice(0)) deliver();
    };
    const RealWorker = window.Worker;
    const HeldWorker = class extends RealWorker {
      addEventListener(
        type: string,
        listener: EventListenerOrEventListenerObject,
        options?: boolean | AddEventListenerOptions
      ): void {
        if (type !== "message") {
          super.addEventListener(type, listener, options);
          return;
        }
        super.addEventListener(
          type,
          (event) => {
            const deliver = () => {
              if (typeof listener === "function") listener.call(this, event);
              else listener.handleEvent(event);
            };
            if (released) deliver();
            else held.push(deliver);
          },
          options
        );
      }
    };
    const handlers = new WeakMap<Worker, ((event: MessageEvent) => unknown) | null>();
    Object.defineProperty(HeldWorker.prototype, "onmessage", {
      configurable: true,
      get(this: Worker) {
        return handlers.get(this) ?? null;
      },
      set(this: Worker, handler: ((event: MessageEvent) => unknown) | null) {
        if (!handlers.has(this)) {
          this.addEventListener("message", (event) => handlers.get(this)?.call(this, event));
        }
        handlers.set(this, handler);
      },
    });
    window.Worker = HeldWorker;
  });
}

/** Turns the browser's scroll anchoring off on `page`, as in Safari before 27, which has none.
 *  Every engine Playwright runs anchors the scroll on the target when a diagram above it grows,
 *  so with anchoring on a row would pass without the page's own focus re-run; with it off, only
 *  that re-run brings the target back into view. */
export async function withoutScrollAnchoring(page: Page): Promise<void> {
  await page.addStyleTag({ content: "* { overflow-anchor: none !important; }" });
}

/** The document's scroll offset once it has stopped moving: unchanged over 10 frames in a row, so
 *  a smooth scroll a link or a wheel started has ended before the offset is read. */
export function settledScrollTop(page: Page): Promise<number> {
  return page.evaluate(
    () =>
      new Promise<number>((resolve) => {
        let last = -1;
        let still = 0;
        const sample = () => {
          const top = document.scrollingElement?.scrollTop ?? 0;
          still = top === last ? still + 1 : 0;
          last = top;
          if (still >= 10) resolve(top);
          else requestAnimationFrame(sample);
        };
        sample();
      })
  );
}

/** Makes the page's `new Worker(...)` throw, as a browser with no worker to give does. */
export async function refuseWorkers(page: Page): Promise<void> {
  await page.addInitScript(() => {
    window.Worker = class {
      constructor() {
        throw new Error("This test's page has no Worker");
      }
    } as unknown as typeof Worker;
  });
}

/** Wraps the page's `Worker` so its workers pass everything through until
 *  `window.__failMermaidWorkers()` runs, and from then on fail as a worker whose chunk can no
 *  longer load does, as across a redeploy: each worker's next `postMessage` posts nothing and
 *  instead fires `error` on that worker (on a later task, as the browser reports a load failure),
 *  and every `new Worker(...)` throws. */
export async function failMermaidWorkersOnCall(page: Page): Promise<void> {
  await page.addInitScript(() => {
    let failing = false;
    window.__failMermaidWorkers = () => {
      failing = true;
    };
    const RealWorker = window.Worker;
    window.Worker = class extends RealWorker {
      constructor(url: string | URL, options?: WorkerOptions) {
        if (failing) throw new Error("This test's worker chunk no longer loads");
        super(url, options);
      }

      postMessage(message: unknown, options?: StructuredSerializeOptions | Transferable[]): void {
        if (failing) {
          setTimeout(() => this.dispatchEvent(new Event("error")), 0);
          return;
        }
        super.postMessage(message, options as StructuredSerializeOptions);
      }
    };
  });
}

/** The built Mermaid render worker's chunk, as Vite emits it under `/assets`. */
const workerChunk = /\/assets\/mermaid-render\.worker-[^/]+\.js$/;

/** The same-origin path the held worker requests once it has taken a diagram's source. */
const workerHeldPath = "/e2e/mermaid-worker-held";

/** What a held worker says about the job it took before it holds it: `"drawing"`, the phase the
 *  real worker posts once a renderer is drawing, which arms the page's render bound
 *  (`RENDER_TIME_LIMIT_MS`); or `"none"`, a worker that takes a job and never says it has begun,
 *  which leaves that job under the renderer's load bound (`RENDERER_LOAD_LIMIT_MS`). */
export type HeldPhase = "drawing" | "none";

/** Prepended to the worker chunk, so it registers before the worker's own listener: it stops that
 *  listener, posts `phase` for the job it took (so the page arms the bound the real worker would
 *  have armed, since the worker's own listener never runs and posts nothing), reports through
 *  `workerHeldPath` that it holds the source, and never returns. */
function holdingListener(phase: HeldPhase): string {
  return [
    'self.addEventListener("message", async (event) => {',
    "  event.stopImmediatePropagation();",
    ...(phase === "none" ? [] : [`  self.postMessage({ id: event.data.id, phase: "${phase}" });`]),
    `  await fetch("${workerHeldPath}");`,
    "  for (;;) {}",
    "});",
  ].join("\n");
}

/** Serves the real worker chunk to `page` with `holdingListener` in front of the worker's own, so
 *  the worker takes a diagram's source and never answers, under the bound `phase` names: the
 *  shipped render limit by default, which refuses the source whatever the machine's speed, or the
 *  renderer's load limit under `"none"`. Returns how many times a worker reported that hold. */
export async function holdWorkerSource(
  page: Page,
  phase: HeldPhase = "drawing"
): Promise<() => number> {
  const listener = holdingListener(phase);
  let holds = 0;
  await page.route(workerChunk, async (route) => {
    const response = await route.fetch();
    await route.fulfill({ response, body: `${listener}\n${await response.text()}` });
  });
  await page.route(
    (url) => url.pathname === workerHeldPath,
    async (route) => {
      holds += 1;
      await route.fulfill({ status: 204 });
    }
  );
  return () => holds;
}

/** Waits for a worker to report that it has taken a diagram's source and holds it, which is also
 *  what says the page handed that source over rather than drawing it on its own thread. */
export async function expectWorkerHeld(holds: () => number): Promise<void> {
  await expect
    .poll(holds, { message: "the Mermaid worker took the diagram's source and holds it" })
    .toBeGreaterThanOrEqual(1);
}

/** Waits for a worker to report that it holds the source, then shows that five 100 ms timers on
 *  the page's thread each fire on time, give or take a scheduling hiccup, while the worker is
 *  busy: a page that drew on its own thread would never hand the source over. */
export async function expectHeldAndResponsive(page: Page, holds: () => number): Promise<void> {
  await expectWorkerHeld(holds);
  for (let sample = 0; sample < 5; sample += 1) {
    const elapsed = await page.evaluate(
      () =>
        new Promise<number>((resolve) => {
          const start = performance.now();
          setTimeout(() => resolve(performance.now() - start), 100);
        })
    );
    expect(elapsed).toBeLessThan(1_000);
  }
}

/** Mermaid's code-split chunks, as Vite names them (`web/vite.config.ts`); the worker's own entry
 *  chunk is `workerChunk` and is not one of these. */
const mermaidChunks = /\/assets\/mermaid-chunk-[^/]+\.js$/;

/** Holds the first of Mermaid's chunks `context` asks for back by `delayMs`, and lets every
 *  later one through at once, so one source waits that long for its renderer and the rest of the
 *  run pays nothing. The route is the context's, not a page's: the chunks are a Web Worker's own
 *  dynamic imports. Returns how many of those chunks were asked for, so a row can read that the
 *  delay was on the path its source took. */
export async function delayMermaidChunks(
  context: BrowserContext,
  delayMs: number
): Promise<() => number> {
  let asked = 0;
  let held = false;
  await context.route(mermaidChunks, async (route) => {
    asked += 1;
    if (!held) {
      held = true;
      await sleep(delayMs);
    }
    await route.continue();
  });
  return () => asked;
}

/** Records the chunks `context` asks for under `/assets`: Mermaid's own code-split chunks, and
 *  the worker's own entry chunk, which is not one of them. The route answers with the real
 *  response, so nothing is held back and a page draws as it would without it. The route is the
 *  context's, not a page's, because Mermaid's chunks are a Web Worker's own dynamic imports. */
export async function recordMermaidChunks(
  context: BrowserContext
): Promise<() => { chunks: string[]; workerEntries: string[] }> {
  const chunks: string[] = [];
  const workerEntries: string[] = [];
  await context.route(mermaidChunks, async (route) => {
    chunks.push(route.request().url());
    await route.continue();
  });
  await context.route(workerChunk, async (route) => {
    workerEntries.push(route.request().url());
    await route.continue();
  });
  return () => ({ chunks, workerEntries });
}

/** Which renderer drew the diagram mounted in `preview`'s open shadow root, read from the sheet
 *  the drawing carries: Mermaid prefixes every rule that has a selector with the drawing's own id
 *  (`#dispatch-mermaid-<n>`, `mermaid-upstream.ts`), where beautiful-mermaid's sheet styles `text`
 *  with its own font and prefixes nothing. `"neither"` where the host holds no drawing or one
 *  whose sheet is neither renderer's, so a row's failure says what it found. */
export function rendererOf(preview: Locator): Promise<MermaidRendererName | "neither"> {
  return preview.evaluate((host) => {
    const drawing = host.shadowRoot?.querySelector("svg");
    if (!drawing) return "neither";
    const sheet = Array.from(drawing.querySelectorAll("style"), (style) => style.textContent).join(
      "\n"
    );
    if (/#dispatch-mermaid-\d+/.test(sheet)) return "mermaid";
    if (sheet.includes("font-family: 'Inter'")) return "beautiful-mermaid";
    return "neither";
  });
}
