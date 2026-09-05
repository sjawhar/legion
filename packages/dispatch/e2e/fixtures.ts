import { fileURLToPath } from "node:url";
import { test as base } from "@playwright/test";
import { type FixtureServer, startFixtureServer } from "./fixture-server";
import { fixtureIssues } from "./threads";

declare global {
  interface Window {
    /** Clipboard writes the page made, recorded by the fixture's init script. */
    readonly __copied: string[];
  }
}

// Each test gets its own fixture backend seeded from `fixtureIssues`, serving
// the built SPA from web/dist (run `bun run build:web` first).
export const test = base.extend<{ dashboard: FixtureServer }>({
  dashboard: async ({ page }, use) => {
    const server = await startFixtureServer({
      distDir: fileURLToPath(new URL("../web/dist", import.meta.url)),
      issues: fixtureIssues,
    });
    // Record clipboard writes instead of touching the real clipboard.
    await page.addInitScript(() => {
      const copied: string[] = [];
      Object.defineProperty(window, "__copied", { value: copied });
      Object.defineProperty(navigator, "clipboard", {
        value: { writeText: async (text: string) => void copied.push(text) },
      });
    });
    await use(server);
    await server.stop();
  },
});

export { expect } from "@playwright/test";
