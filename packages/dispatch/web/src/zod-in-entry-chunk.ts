import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

/**
 * The entry chunk is what runs before any of the page's own work: the browser downloads it,
 * compiles it and evaluates every static import of it in one main-thread task, measured at
 * about a second on a phone at 4x CPU throttling, and a document's first block is behind it
 * (dispatch://INFRA-341, R11, acceptance A11). About a sixth of that task was zod building
 * schemas the dashboard never parses with: it imports `@legion/contracts` for its types and for
 * plain values, each of which now has a subpath of its own that builds no schema as it loads
 * (`packages/contracts/src/dispatch-constants.ts`, and the aliases in `web/vite.config.ts`).
 *
 * One import of the package's index from a module the entry reaches would put all of it back,
 * and nothing in the source says so. This check reads the build instead: the module script
 * `index.html` loads, and every chunk the browser preloads with it - which is the entry's own
 * static-import closure, the modules of that same first task - must hold none of zod's runtime.
 * A lazy chunk may hold it: it is downloaded and evaluated when the page asks for it, not here.
 */
const DIST_DIR = join(import.meta.dir, "..", "dist");

/**
 * What zod 4 leaves in a bundle whatever the minifier does to it. `_zod` is the property every
 * schema carries its internals under, read and written by name throughout the library, and the
 * `$Zod*` names are its base classes; a minifier renames neither a property nor a string. Any
 * one of them in the entry chunk is zod in the entry chunk.
 */
export const ZOD_MARKERS = ["_zod", "$ZodType", "$ZodError", "$ZodCheck"] as const;

export interface ZodInChunk {
  /** The file's name below `dist`, as `index.html` names it. */
  readonly file: string;
  /** Each marker found, with how many times it occurs. */
  readonly markers: readonly { readonly marker: string; readonly count: number }[];
}

/**
 * The scripts the browser evaluates before anything the page does: the one module script
 * `index.html` loads and every `modulepreload` beside it, in the order the HTML names them.
 * Vite emits a preload link for each chunk the entry statically imports, so this is the whole
 * of that first task's code.
 */
export function entryScripts(html: string): string[] {
  const scripts = [
    ...html.matchAll(/<script\b[^>]*\btype="module"[^>]*\bsrc="([^"]+)"/g),
    ...html.matchAll(/<link\b[^>]*\brel="modulepreload"[^>]*\bhref="([^"]+)"/g),
  ].map(([, src]) => (src as string).replace(/^\//, ""));
  return [...new Set(scripts)];
}

/** Which of `ZOD_MARKERS` `code` holds, and how often; empty for a chunk with no zod in it. */
export function zodMarkersIn(code: string): ZodInChunk["markers"] {
  return ZOD_MARKERS.map((marker) => ({
    marker,
    count: code.split(marker).length - 1,
  })).filter((found) => found.count > 0);
}

/** Every entry script of the build in `dist` that holds zod, in the order the HTML names them. */
export function zodInEntryChunks(dist: string): ZodInChunk[] {
  const html = readFileSync(join(dist, "index.html"), "utf8");
  const scripts = entryScripts(html);
  if (scripts.length === 0) {
    throw new Error(
      `${join(dist, "index.html")} names no module script: there is no entry chunk to check.`
    );
  }
  const found: ZodInChunk[] = [];
  for (const file of scripts) {
    const markers = zodMarkersIn(readFileSync(join(dist, file), "utf8"));
    if (markers.length > 0) {
      found.push({ file, markers });
    }
  }
  return found;
}

function main(): void {
  if (!existsSync(join(DIST_DIR, "index.html"))) {
    throw new Error(
      `No build found at ${DIST_DIR}. Run "bun run build:web" before "bun run check:no-zod".`
    );
  }
  const carrying = zodInEntryChunks(DIST_DIR);
  for (const chunk of carrying) {
    console.error(
      `web/dist/${chunk.file} holds zod (${chunk.markers
        .map((found) => `${found.marker} ×${found.count}`)
        .join(", ")}): a module the entry chunk reaches imports "@legion/contracts" itself, ` +
        "rather than one of its subpaths that builds no schema as it loads."
    );
  }
  if (carrying.length > 0) {
    process.exit(1);
  }
  const scripts = entryScripts(readFileSync(join(DIST_DIR, "index.html"), "utf8"));
  console.log(
    `No zod in the ${scripts.length} script(s) the page evaluates first: ${scripts.join(", ")}.`
  );
}

if (import.meta.main) {
  main();
}
