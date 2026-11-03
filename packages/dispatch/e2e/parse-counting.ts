/**
 * Counts the markdown parses a built page makes, in either of the browser's two engines.
 *
 * How the counter reaches the built page: the dashboard the server serves is a Vite build, so no
 * hook of this suite is inside it. This rewrites every JavaScript asset the browser asks for as
 * it is served (`context.route`), turning each call of a parse function into the same call
 * through a counting property name:
 *
 *   engine.parseMarkdown(markdown, softBreaks)  ->  engine[count("display")](markdown, softBreaks)
 *   editor.setMarkdown(markdown)                ->  editor[count("editor")](markdown)
 *
 * A computed member call keeps its receiver, so each engine runs exactly as it does unrewritten,
 * and `count` returns the property name it was asked to count. `parseMarkdown` is the one entry
 * of the read-only engine every display surface parses through
 * (`features/refs/markdown-engine.ts`), and `setMarkdown` is the one entry of the editing
 * engine's markdown import (`createProofEditor`'s handle, which builds a `ParserState` of its
 * own): between them they are every markdown parse a browser engine makes on a page that only
 * reads. The Milkdown node specs' own `parseMarkdown:` fields are declarations, never calls
 * through a receiver, so the rewrite leaves them alone.
 *
 * The parse worker runs the read-only engine in a thread of its own, whose `globalThis` is not
 * the page's, and a body the page posts to it is one parse there. So the counter wraps the
 * `postMessage` of every worker built from the parse worker's own bundle: zero bodies posted is
 * zero parses in the worker, whether or not that bundle was rewritten too.
 *
 * `rewrittenParseCallSites` is how many call sites the rewrite replaced: a build that renames
 * them leaves it at zero, so a row that asserts it is above zero cannot pass vacuously.
 */
import type { BrowserContext, Page } from "@playwright/test";

/** The parses one page made, by the engine that made them. */
export interface ParseCounts {
  /** Calls of the read-only engine's `parseMarkdown` on the page's own thread. */
  readonly display: number;
  /** Calls of the editing engine's `setMarkdown`, its markdown import. */
  readonly editor: number;
  /** Bodies the page posted to the parse worker, one parse of its engine apiece. */
  readonly workerRequests: number;
}

/** What the prelude keeps on the page, which `readParseCounts` reads back. */
interface PageCounts {
  display: number;
  editor: number;
  workerRequests: number;
}

declare global {
  interface Window {
    __dispatchParseCounts?: PageCounts;
  }
}

/** The prelude every rewritten asset carries, so the first one to run defines the counter for the
 *  page and for the parse worker alike. */
const prelude =
  "globalThis.__dispatchParseCounts ??= { display: 0, editor: 0, workerRequests: 0 };" +
  "globalThis.__dispatchCountParse ??= (kind) => {" +
  "globalThis.__dispatchParseCounts[kind] += 1;" +
  'return kind === "editor" ? "setMarkdown" : "parseMarkdown";' +
  "};";

/** `source` with every parse call site counted, and how many were rewritten. */
function counted(source: string): { code: string; rewritten: number } {
  let rewritten = 0;
  const code = source.replace(/\.(parseMarkdown|setMarkdown)\(/g, (_match, name: string) => {
    rewritten += 1;
    const kind = name === "setMarkdown" ? "editor" : "display";
    return `[globalThis.__dispatchCountParse("${kind}")](`;
  });
  return { code: rewritten === 0 ? source : `${prelude}\n${code}`, rewritten };
}

/** How many call sites the rewrite replaced in the assets each context was served. */
const rewrittenCallSites = new WeakMap<BrowserContext, number>();

/**
 * Counts every markdown parse the pages of `context` make, from now on. Install it before the
 * first page is opened: an asset already in the browser's cache is served unrewritten.
 */
export async function countParses(context: BrowserContext): Promise<void> {
  // The counters exist before the first module of the page runs, so a page that parsed nothing
  // reads zero rather than undefined, and the parse worker is wrapped from its construction on.
  await context.addInitScript(() => {
    window.__dispatchParseCounts ??= { display: 0, editor: 0, workerRequests: 0 };
    const counts = window.__dispatchParseCounts;
    const built = window.Worker;
    const parseWorkers = new WeakSet<Worker>();
    // The DOM's `postMessage` is overloaded; this one signature stands for both its forms, and
    // the wrapper passes on exactly what it was given.
    const post = built.prototype.postMessage as (
      this: Worker,
      message: unknown,
      transfer?: unknown
    ) => void;
    built.prototype.postMessage = function postMessageCounted(
      this: Worker,
      message: unknown,
      transfer?: unknown
    ): void {
      if (parseWorkers.has(this)) counts.workerRequests += 1;
      post.call(this, message, transfer);
    } as Worker["postMessage"];
    window.Worker = class CountedWorker extends built {
      constructor(script: string | URL, options?: WorkerOptions) {
        super(script, options);
        if (/markdown-parse\.worker/.test(String(script))) parseWorkers.add(this);
      }
    };
  });
  await context.route(/\.js(\?|$)/, async (route) => {
    const response = await route.fetch();
    const { code, rewritten } = counted(await response.text());
    const headers = { ...response.headers() };
    // The body is the decoded one, so what the response said about its encoding and its length
    // no longer holds.
    delete headers["content-encoding"];
    delete headers["content-length"];
    rewrittenCallSites.set(context, (rewrittenCallSites.get(context) ?? 0) + rewritten);
    await route.fulfill({ body: code, headers, status: response.status() });
  });
}

/** How many parse call sites `countParses` has rewritten for `context` so far. */
export function rewrittenParseCallSites(context: BrowserContext): number {
  return rewrittenCallSites.get(context) ?? 0;
}

/** The parses `page` has made since it was opened. */
export function readParseCounts(page: Page): Promise<ParseCounts> {
  return page.evaluate(
    () => window.__dispatchParseCounts ?? { display: 0, editor: 0, workerRequests: 0 }
  );
}
