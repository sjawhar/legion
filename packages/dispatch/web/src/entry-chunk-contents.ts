import { existsSync, readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";

import { entryScripts } from "./zod-in-entry-chunk";

/**
 * What the entry chunk may hold - React, react-dom, the router, the query client and the
 * shell's frame - and which parts of the app evaluate in a main-thread task of their own.
 *
 * The browser downloads a chunk, compiles it and evaluates every static import of it in one
 * main-thread task: a static module graph evaluates in one task whatever it holds, and a task
 * over 500 ms is a freeze a reader feels (dispatch://INFRA-341, R11). So this check reads two
 * things from the build, both of them about which modules share a task.
 *
 * The first is the entry chunk, the task that runs before anything of the page's own. With the
 * app's own source in it that task took 501 to 655 ms on a phone at 4x CPU throttling, so the
 * app is asked for with `import()` instead (`main.tsx`), and so is every part below: each is
 * drawn by a route, by a tab the reader opens or by a key of their own, not by the shell's
 * first frame. One static import from a module the entry reaches would put it back, and
 * nothing in the source says so.
 *
 * The second is `apartFrom`: the route that opens a document used to evaluate its whole page -
 * the Conversation, the Activity list, the role documents and the document surface - in one
 * task of 505 to 579 ms, because its chunk imported all of them statically. A part named there
 * must not be in the static-import closure of the part it is named against, so the browser
 * evaluates the two in tasks of their own.
 *
 * This check reads the build rather than the source, as `check:no-zod` does: the module script
 * `index.html` loads and every chunk the browser preloads with it - the entry's own
 * static-import closure, the modules of that same first task - must hold none of these parts'
 * markers, and no part may share a task with one it is named apart from.
 *
 * It reads the other half of the split too. Moving a part a document address needs into a chunk
 * asked for only once the entry has evaluated trades one wait for another, so the build writes
 * the chunks of those parts into `index.html` for the page to preload
 * (`documentAddressChunks` in `web/vite.config.ts`): a part a list names in `neededBy` must be
 * in a chunk that list names.
 */
const DIST_DIR = join(import.meta.dir, "..", "dist");

/**
 * The address shapes `index.html`'s inline script preloads for, as the build writes them into
 * `window.__dispatchAddressChunks` (`documentAddressChunks` in web/vite.config.ts). `app` is
 * the list every address that opens no document reads, and the two document lists begin with
 * everything in it, since the script reads one list and only one.
 */
export type PreloadAddress = "app" | "document" | "issue";

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
   *  therefore name its chunk; empty for a part no address draws first. */
  readonly neededBy: readonly PreloadAddress[];
  /** Where the part is loaded from, for the message a failure prints. */
  readonly loadedBy: string;
  /**
   * The parts whose own chunk must not reach this one by static import. A chunk evaluates only
   * once everything it statically imports has, so the two would be one main-thread task; named
   * here, they are two.
   */
  readonly apartFrom?: readonly string[];
}

export const DEFERRED_PARTS: readonly DeferredPart[] = [
  {
    loadedBy: "`App` in web/src/main.tsx",
    // `data-shell-layout` is the attribute the shell writes on <main>, and a minifier renames
    // no property name: the app's own module carries it, so a build with the app in the entry
    // chunk carries it there.
    marker: "data-shell-layout",
    name: "the app and its routes",
    neededBy: ["app", "document", "issue"],
  },
  {
    loadedBy: "`App` in web/src/main.tsx, through `app.tsx`",
    marker: "Peek at the inbox without leaving this page",
    name: "the sidebar",
    neededBy: ["app", "document", "issue"],
  },
  {
    loadedBy: "`App` in web/src/main.tsx, through `app.tsx`",
    // The prefix every kept write is stored under (`features/writes/write-storage.ts`).
    marker: "dispatch.write:",
    name: "the page's write store",
    neededBy: ["app", "document", "issue"],
  },
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
    apartFrom: ["the issue page", "the project document page"],
    loadedBy:
      "`ArtifactDocument` in web/src/features/issue/IssuePage.tsx and " +
      "web/src/features/document/DocumentPage.tsx",
    marker: "Document editor",
    name: "the document surface",
    neededBy: ["document", "issue"],
  },
  {
    apartFrom: ["the issue page"],
    loadedBy: "`ConversationTab` in web/src/features/issue/IssuePage.tsx",
    marker: "Conversation turns",
    name: "the issue page's Conversation",
    // Not preloaded: a document address opens the Spec or an artifact, never the Conversation,
    // and the tab's chunk is asked for when the reader opens the tab.
    neededBy: [],
  },
  {
    apartFrom: ["the issue page"],
    loadedBy: "`ActivityTab` in web/src/features/issue/IssuePage.tsx",
    marker: "Issue activity",
    name: "the issue page's Activity",
    neededBy: [],
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
 * A chunk's static import of another chunk, as the build writes it: `import"./other.js"` and
 * `… from"./other.js"`, never `import("./other.js")`, which is a chunk of a later task. The
 * specifier has to be relative and a chunk, so a sentence in a string that happens to read
 * `from "…"` names nothing.
 */
const STATIC_IMPORT = /(?:\bfrom|(?<![\w$)\]])import)\s*"(\.[^"]*\.js)"/g;

/**
 * The chunks the browser evaluates in one main-thread task with `file`, `file` included: the
 * chunks it statically imports, theirs, and so on, since a module graph is evaluated only once
 * all of it has arrived. A name this build does not hold ends that branch rather than throwing:
 * the preload rule below reports a missing file, and this one is about what shares a task.
 */
export function oneTaskWith(dist: string, file: string): Set<string> {
  const reached = new Set<string>();
  const walk = (name: string): void => {
    if (reached.has(name)) return;
    reached.add(name);
    const path = join(dist, name);
    if (!existsSync(path)) return;
    for (const [, specifier] of readFileSync(path, "utf8").matchAll(STATIC_IMPORT)) {
      walk(join(dirname(name), specifier));
    }
  };
  walk(file);
  return reached;
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
  const holdersOf = new Map<string, string[]>(
    DEFERRED_PARTS.map((part) => [part.name, chunksHolding(dist, part.marker)])
  );
  for (const part of DEFERRED_PARTS) {
    const holders = holdersOf.get(part.name) ?? [];
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
    for (const other of part.apartFrom ?? []) {
      const otherHolders = holdersOf.get(other);
      if (otherHolders === undefined) {
        throw new Error(
          `${part.name} is named apart from "${other}", which is no part of DEFERRED_PARTS`
        );
      }
      for (const otherHolder of otherHolders) {
        const oneTask = oneTaskWith(dist, otherHolder);
        const shared = holders.filter((file) => oneTask.has(file));
        if (shared.length === 0) {
          continue;
        }
        faults.push({
          fault:
            `${otherHolder} reaches ${shared.join(", ")} by static import, so the browser ` +
            `evaluates ${other} and ${part.name} in one main-thread task: ${other} imports it ` +
            `statically, rather than leaving it to ${part.loadedBy}`,
          part,
        });
      }
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
      `the page evaluates first (${scripts.join(", ")}), each part named apart from another ` +
      "evaluates in a task of its own, and an address preloads the chunks of the ones it needs."
  );
}

if (import.meta.main) {
  main();
}
