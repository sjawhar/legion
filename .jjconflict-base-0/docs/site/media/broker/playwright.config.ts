// docs/site/media/broker/playwright.config.ts
//
// Runs the broker's media specs against a rig already up: rig.sh starts every server and the
// agent machine, then runs Playwright as its command, so this config starts no server of its own.
// screenshots.sh and walkthrough.sh are the entry points, each naming its one spec.
import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: ".",
  testMatch: /(screenshots\.spec|walkthrough\.record)\.ts/,
  outputDir: "test-results",
  fullyParallel: false,
  workers: 1,
  reporter: "list",
  expect: { timeout: 30_000 },
  use: { ...devices["Desktop Chrome"], headless: true, trace: "retain-on-failure" },
});
