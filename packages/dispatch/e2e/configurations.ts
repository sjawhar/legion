/** The configuration each Playwright project runs its rows in, and the rows a `--list` run names,
 *  so a row that runs twice in one configuration can be found.
 *
 * A configuration is what a repeat is counted by: the engine, the viewport, the origin with its
 * secure context, and how many Dispatch server processes the run has. Two projects may share a
 * configuration without repeating anything - `chromium` and `chromium-alone` do (the projects of
 * those names in e2e/playwright.config.ts), and so do `firefox` and `firefox-download-navigation`
 * - because they run disjoint files. A repeat is the same `(configuration, file, title)` twice,
 * which `duplicateRows` below reports.
 *
 * Nothing here imports Playwright: the values come from a config object the caller passes and
 * from a listing's JSON, so every function is callable from a plain Bun test.
 */

/** A row of `playwright test --list --reporter=json`: one test of one project. */
export type ListedRow = {
  /** The spec file, as the listing names it: relative to the config's root directory. */
  file: string;
  /** The project that runs the row, by `name` in the config. */
  project: string;
  /** The row's title, its `describe` titles first, joined by the character Playwright joins a
   *  title path with. */
  title: string;
};

/** A `(configuration, file, title)` more than one project runs. */
export type Duplicate = {
  /** The configuration key the projects share. */
  configuration: string;
  file: string;
  /** Every project that runs the row, in listing order. */
  projects: string[];
  title: string;
};

/** The `use` values a configuration is read from.
 *
 * `defaultBrowserType` is the engine a `devices[...]` entry carries
 * (playwright-core/types/types.d.ts:18944). Playwright reads `browserName` from it when a project
 * sets no `browserName`, and defaults it to `chromium`
 * (playwright/lib/index.js:187-188), so this module resolves the engine the same way. Playwright's
 * public `TestOptions` does not name `defaultBrowserType`, which is why this type does rather than
 * reusing it. */
export type ConfigurationUse = {
  baseURL?: string;
  browserName?: string;
  defaultBrowserType?: string;
  viewport?: { height: number; width: number } | null;
};

/** As much of a Playwright config as a configuration is read from: the top-level `use` every
 *  project inherits, and the projects that override it key by key. */
export type ProjectsConfig = {
  projects?: readonly { name?: string; use?: ConfigurationUse }[];
  use?: ConfigurationUse;
};

/** The shape of `playwright test --list --reporter=json`'s output that this module reads. */
export type Listing = {
  config?: { projects?: readonly { name?: string }[] };
  suites?: readonly ListingSuite[];
};

type ListingSuite = {
  specs?: readonly ListingSpec[];
  suites?: readonly ListingSuite[];
  title?: string;
};

type ListingSpec = {
  file: string;
  tests?: readonly { projectName?: string }[];
  title: string;
};

/** Playwright's own viewport when neither the project nor the config sets one: "Defaults to an
 *  1280x720 viewport" (playwright-core/types/types.d.ts:26679). `viewport: null` is a different
 *  thing - no fixed viewport at all - and leaves a project with no configuration here. */
const defaultViewport = { height: 720, width: 1280 };

/** What joins a row's `describe` titles to its own, so two rows of one file whose own titles
 *  match but whose blocks differ stay two rows. Playwright's own reports read this way. */
const titleSeparator = " › ";

/** Whether a page at `origin` is a secure context, which decides what the browser gives it:
 *  `navigator.locks` and `crypto.randomUUID` on a secure one, neither on a plain-HTTP name that is
 *  not loopback, which is what the `chromium-plain-http*` projects are for
 *  (e2e/plain-http-origin.ts:3-9). */
function secureContext(origin: URL): boolean {
  if (origin.protocol === "https:") return true;
  const host = origin.hostname;
  return (
    host === "localhost" ||
    host.endsWith(".localhost") ||
    host === "[::1]" ||
    /^127\.\d+\.\d+\.\d+$/.test(host)
  );
}

/** The configuration `use` puts a project's rows in, or `undefined` when it names no configuration
 *  this can read: no origin to open, an origin that is no URL, or `viewport: null`, which leaves
 *  the window whatever size the browser opens. A project the key cannot be read for is left out of
 *  `projectConfigurations` below, so the test that lists the projects names it. */
function configurationKey(use: ConfigurationUse, processes: number): string | undefined {
  const engine = use.browserName ?? use.defaultBrowserType ?? "chromium";
  const viewport = use.viewport === undefined ? defaultViewport : use.viewport;
  if (viewport === null || use.baseURL === undefined) return undefined;
  let origin: URL;
  try {
    origin = new URL(use.baseURL);
  } catch {
    return undefined;
  }
  const context = secureContext(origin) ? "secure" : "insecure";
  return `${engine} ${viewport.width}x${viewport.height} ${origin.origin} ${context} ${processes}-process`;
}

/** Every project of `config` by name, with the configuration its rows run in. `processes` is how
 *  many Dispatch server processes the run has (e2e/harness-processes.ts), which is also what
 *  chooses the project list (the config's `projects:`, e2e/playwright.config.ts). One listing is
 *  one run, so every key of it carries the same process count and that segment can never tell two
 *  of its rows apart. It is in the key because it is the only place the one-against-two-process
 *  distinction is written down, and a failure prints the key, so the reader sees which run's
 *  projects were compared.
 *
 * A project whose configuration cannot be read, and one with no `name` at all, is left out rather
 * than given a guessed key: the caller sees a project the map has none for. */
export function projectConfigurations(
  config: ProjectsConfig,
  processes: number
): Map<string, string> {
  const configurations = new Map<string, string>();
  for (const project of config.projects ?? []) {
    if (project.name === undefined) continue;
    // A project's `use` overrides the config's key by key, so the configuration is read from the
    // merge, not from either alone: `iphone` takes its viewport from the device it spreads and its
    // origin from the config's top-level `use` above it (e2e/playwright.config.ts).
    const key = configurationKey({ ...config.use, ...project.use }, processes);
    if (key !== undefined) configurations.set(project.name, key);
  }
  return configurations;
}

/** Every project the listing names, whether or not any row fell to it: Playwright reports its
 *  resolved config, so a project whose files are all filtered out is in here too. */
export function listedProjects(listing: Listing): string[] {
  const names = new Set<string>();
  for (const project of listing.config?.projects ?? []) names.add(project.name ?? "");
  return [...names].sort();
}

function collectRows(suite: ListingSuite, titles: readonly string[], rows: ListedRow[]): void {
  for (const spec of suite.specs ?? []) {
    const title = [...titles, spec.title].join(titleSeparator);
    for (const test of spec.tests ?? []) {
      if (test.projectName === undefined) {
        throw new Error(`a listed row of ${spec.file} names no project: ${title}`);
      }
      rows.push({ file: spec.file, project: test.projectName, title });
    }
  }
  for (const child of suite.suites ?? []) {
    collectRows(child, [...titles, child.title ?? ""], rows);
  }
}

/** Every row the listing names, one per project a spec runs in. A top-level suite is a file and
 *  its title is that file's path, so a row's title starts at the `describe` blocks below it. */
export function rowsOfListing(listing: Listing): ListedRow[] {
  const rows: ListedRow[] = [];
  for (const file of listing.suites ?? []) collectRows(file, [], rows);
  return rows;
}

/** Each `(configuration, file, title)` that more than one of `rows` holds: the rows that run twice
 *  in one configuration. `keyOf` answers the configuration a row runs in, which keeps this
 *  function free of any knowledge of projects and of Playwright. */
export function duplicateRows<Row extends ListedRow>(
  rows: Iterable<Row>,
  keyOf: (row: Row) => string
): Duplicate[] {
  const seen = new Map<string, Duplicate>();
  for (const row of rows) {
    const configuration = keyOf(row);
    // JSON is the separator-free way to key three strings: no title, file or configuration can
    // run into the next one.
    const id = JSON.stringify([configuration, row.file, row.title]);
    const found = seen.get(id);
    if (found === undefined) {
      seen.set(id, { configuration, file: row.file, projects: [row.project], title: row.title });
      continue;
    }
    found.projects.push(row.project);
  }
  return [...seen.values()].filter((duplicate) => duplicate.projects.length > 1);
}
