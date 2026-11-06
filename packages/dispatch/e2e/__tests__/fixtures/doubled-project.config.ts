import { fileURLToPath } from "node:url";
import { defineConfig } from "@playwright/test";

import realConfig from "../../playwright.config";

/** The negative control of e2e/__tests__/row-configurations.test.ts: the real config with one
 *  project added that runs a spec file `chromium` already runs, in `chromium`'s own `use`, so
 *  every row of that file runs twice in one configuration. A listing of this config must yield
 *  the duplicates a listing of the real one must not; without it, a test that finds nothing
 *  would read the same whether it looked or not.
 *
 * Playwright resolves `testDir` against the directory its config file is in, so this one names
 * the `e2e` directory that the real config's `testDir: "."` (e2e/playwright.config.ts) means.
 * Everything else - `testMatch`, `testIgnore`, `use` and every project - is the real config's.
 */
const e2eDirectory = fileURLToPath(new URL("../..", import.meta.url));

/** The file whose rows the added project repeats. `chromium` runs it: it is no plain-HTTP,
 *  multiprocess or clock spec, which are the files that project ignores (its `testIgnore`,
 *  e2e/playwright.config.ts). */
export const doubledSpec = "activity.e2e.ts";

/** The added project's name. Playwright refuses two projects of one name, so it is not
 *  `chromium`; its `use` is `chromium`'s, which is what puts the two in one configuration. */
export const doubledProject = "chromium-doubled";

const chromium = realConfig.projects?.find((project) => project.name === "chromium");
if (chromium === undefined) {
  throw new Error(
    "e2e/playwright.config.ts has no project named `chromium` to double, and this fixture " +
      "repeats that project's `use` so the project it adds shares its configuration."
  );
}

export default defineConfig({
  ...realConfig,
  projects: [
    ...(realConfig.projects ?? []),
    {
      name: doubledProject,
      testMatch: new RegExp(`${doubledSpec.replaceAll(".", "\\.")}$`),
      use: chromium.use,
    },
  ],
  testDir: e2eDirectory,
});
