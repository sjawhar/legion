import { existsSync, readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

import { entryScripts } from "./zod-in-entry-chunk";

/**
 * What the entry chunk may hold: the router shell, and nothing a route draws.
 *
 * The browser downloads that chunk, compiles it and evaluates every static import of it in one
 * main-thread task before anything of the page's own runs - measured at 753 to 961 ms on a phone
 * at 4x CPU throttling - and a document's first editable block is behind it
 * (dispatch://INFRA-341, R11). Each part below is drawn by a route or by a reader's own key, not
 * by the shell's first frame, so each lives in a chunk of its own; one static import from a
 * module the entry reaches would put it back, and nothing in the source says so.
 *
 * This check reads the build instead, as `check:no-zod` does: the module script `index.html`
 * loads and every chunk the browser preloads with it - the entry's own static-import closure, the
 * modules of that same first task - must hold none of these parts' markers.
 *
 * It reads the other half of the split too. Moving a part a document address needs into a chunk
 * asked for only once the entry has evaluated trades one wait for another, so the build writes
 * the chunks of those parts into `index.html` for the page to preload
 * (`documentAddressChunks` in `web/vite.config.ts`): a part marked `onADocumentAddress` must be
 * in a chunk that list names.
 */
const DIST_DIR = join(import.meta.dir, "..", "dist");

/** The two address shapes `index.html`'s inline script preloads for, as the build writes them
 *  into `window.__dispatchAddressChunks` (`documentAddressChunks` in web/vite.config.ts). */
export type DocumentAddress = "document" | "issue";

export interface DeferredPart {
  /** What the part is, in the message a failure prints. */
  readonly name: string;
  /**
   * A string the part's code carries whatever the minifier does to it: an attribute name, a DOM
   * id or a message a reader sees, never an identifier - a minifier renames neither a property
   * nor a string. A marker no chunk of the build holds is a marker that has drifted, which this
   * check refuses rather than passing on.
   */
  readonly marker: string;
  /** The addresses whose reader needs it before the page draws, and whose preload list must
   *  therefore name its chunk; empty for a part no document address draws. */
  readonly neededBy: readonly DocumentAddress[];
  /** Where the part is loaded from, for the message a failure prints. */
  readonly loadedBy: string;
}

export const DEFERRED_PARTS: readonly DeferredPart[] = [
  {
    loadedBy: "`importIssuePage` in web/src/app.tsx",
    marker: "issue-spec-panel",
    name: "the issue page",
    neededBy: ["issue"],
  },
  {
    loadedBy: "`importDocumentPage` in web/src/app.tsx",
    marker: "Document subscribers query requires an artifact.",
    name: "the project document page",
    neededBy: ["document"],
  },
  {
    loadedBy: "web/src/features/doc/DeferredAskCard.tsx",
    marker: "data-dispatch-ask-copied-from",
    name: "the decision card every Markdown body draws",
    // Not preloaded: no `ask` shell exists until a body has been parsed and painted, and this
    // chunk brings the Inbox's ask card with it (`DOCUMENT_ADDRESS_MODULES` says why).
    neededBy: [],
  },
  {
    loadedBy: "`SearchPalette` in web/src/features/shell/deferred-shell.ts",
    marker: "search-group-more-actions",
    name: "the search palette",
    neededBy: [],
  },
  {
    loadedBy: "`InboxList` in web/src/features/inbox/InboxList.tsx",
    marker: "inbox-list",
    name: "the Inbox's list",
    neededBy: [],
  },
  {
    loadedBy: "`MarginColumn` in web/src/features/margin/MarginColumn.tsx",
    marker: "margin-rail",
    name: "the margin",
    neededBy: [],
  },
];

/** Every chunk of the build in `dist` whose code holds `marker`, by path below `dist`. */
export function chunksHolding(dist: string, marker: string): string[] {
  const assets = join(dist, "assets");
  return readdirSync(assets)
    .filter((name) => name.endsWith(".js"))
    .filter((name) => readFileSync(join(assets, name), "utf8").includes(marker))
    .map((name) => `assets/${name}`)
    .sort();
}

/**
 * The chunks `index.html` preloads for each address shape, by path below `dist`: what
 * `documentAddressChunks` wrote into the page for its inline script to fetch beside the entry.
 */
export function addressChunks(html: string): Record<string, string[]> {
  const written = /window\.__dispatchAddressChunks = (\{.*?\});/s.exec(html);
  if (written === null) {
    throw new Error(
      "index.html names no __dispatchAddressChunks: the build wrote no chunks for a document " +
        "address to preload (`documentAddressChunks` in web/vite.config.ts)"
    );
  }
  const parsed: unknown = JSON.parse(written[1]);
  if (typeof parsed !== "object" || parsed === null) {
    throw new Error(`index.html's __dispatchAddressChunks is not an object: ${written[1]}`);
  }
  return Object.fromEntries(
    Object.entries(parsed as Record<string, unknown>).map(([address, files]) => [
      address,
      (files as string[]).map((file) => file.replace(/^\//, "")),
    ])
  );
}

export interface BuildFault {
  /** The part the fault is about, or `undefined` for a fault of the page's own preload list. */
  readonly part: DeferredPart | undefined;
  /** What is wrong, as the failure prints it. */
  readonly fault: string;
}

/** Every rule of this check that the build in `dist` breaks: the page's own preload lists
 *  first, then each part in the order they are listed. */
export function faultsInBuild(dist: string): BuildFault[] {
  const html = readFileSync(join(dist, "index.html"), "utf8");
  const entry = new Set(entryScripts(html));
  if (entry.size === 0) {
    throw new Error(`${join(dist, "index.html")} names no module script: there is no entry chunk.`);
  }
  const preloaded = addressChunks(html);
  const faults: BuildFault[] = [];
  for (const [address, files] of Object.entries(preloaded)) {
    for (const file of files) {
      if (!existsSync(join(dist, file))) {
        faults.push({
          fault: `the ${address} address preloads ${file}, which this build does not hold`,
          part: undefined,
        });
      }
    }
  }
  for (const part of DEFERRED_PARTS) {
    const holders = chunksHolding(dist, part.marker);
    if (holders.length === 0) {
      faults.push({
        fault:
          `no chunk of this build holds "${part.marker}", so this check reads nothing: the ` +
          `string has drifted and the rule for ${part.name} is no longer enforced`,
        part,
      });
      continue;
    }
    const inEntry = holders.filter((file) => entry.has(file));
    if (inEntry.length > 0) {
      faults.push({
        fault:
          `${inEntry.join(", ")} is evaluated in the page's first task and holds "${part.marker}": ` +
          `a module the entry chunk reaches imports ${part.name} statically, rather than ` +
          `leaving it to ${part.loadedBy}`,
        part,
      });
    }
    for (const address of part.neededBy) {
      if (holders.some((file) => preloaded[address]?.includes(file))) {
        continue;
      }
      faults.push({
        fault:
          `${holders.join(", ")} holds ${part.name}, which the ${address} address needs, and ` +
          "index.html preloads none of them there: its bytes would be asked for only once the " +
          "entry chunk had evaluated (`DOCUMENT_ADDRESS_MODULES` in web/vite.config.ts)",
        part,
      });
    }
  }
  return faults;
}

function main(): void {
  if (!existsSync(join(DIST_DIR, "index.html"))) {
    throw new Error(
      `No build found at ${DIST_DIR}. Run "bun run build:web" before "bun run check:entry-chunk".`
    );
  }
  const faults = faultsInBuild(DIST_DIR);
  for (const fault of faults) {
    console.error(`${fault.part?.name ?? "index.html"}: ${fault.fault}.`);
  }
  if (faults.length > 0) {
    process.exit(1);
  }
  const scripts = entryScripts(readFileSync(join(DIST_DIR, "index.html"), "utf8"));
  console.log(
    `None of the ${DEFERRED_PARTS.length} deferred parts is in the ${scripts.length} script(s) ` +
      `the page evaluates first (${scripts.join(", ")}), and a document address preloads the ` +
      "chunks of the ones it needs."
  );
}

if (import.meta.main) {
  main();
}
