// The init-script and worker helpers `e2e/mermaid.e2e.ts`'s rows share: the wrappers that record,
// hold, refuse or fail the page's `Worker`, the two scroll helpers (one turns the browser's scroll
// anchoring off, one reads the offset once it settles), and the route that makes the real worker
// chunk hold a diagram's source.
import { expect, type Page } from "@playwright/test";

// What this file's init scripts keep on the page's window.
declare global {
  interface Window {
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

/** Prepended to the worker chunk, so it registers before the worker's own listener: it stops that
 *  listener, reports through `workerHeldPath` that it holds the source, and never returns. */
const holdingListener = [
  'self.addEventListener("message", async (event) => {',
  "  event.stopImmediatePropagation();",
  `  await fetch("${workerHeldPath}");`,
  "  for (;;) {}",
  "});",
].join("\n");

/** Serves the real worker chunk to `page` with `holdingListener` in front of the worker's own, so
 *  the worker takes a diagram's source and never answers, and the page's shipped time limit
 *  refuses it whatever the machine's speed. Returns how many times a worker reported that hold. */
export async function holdWorkerSource(page: Page): Promise<() => number> {
  let holds = 0;
  await page.route(workerChunk, async (route) => {
    const response = await route.fetch();
    await route.fulfill({ response, body: `${holdingListener}\n${await response.text()}` });
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

/** Waits for a worker to report that it holds the source, then shows that five 100 ms timers on
 *  the page's thread each fire on time, give or take a scheduling hiccup, while the worker is
 *  busy: a page that drew on its own thread would never hand the source over. */
export async function expectHeldAndResponsive(page: Page, holds: () => number): Promise<void> {
  await expect
    .poll(holds, { message: "the Mermaid worker took the diagram's source and holds it" })
    .toBeGreaterThanOrEqual(1);
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
