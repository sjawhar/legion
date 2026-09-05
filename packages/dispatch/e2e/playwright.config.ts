import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: ".",
  testMatch: /.*\.e2e\.ts/,
  outputDir: "test-results",
  fullyParallel: true,
  retries: 0,
  reporter: "list",
  use: { ...devices["Desktop Chrome"], headless: true },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
