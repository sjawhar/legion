import { fileURLToPath } from "node:url";
import { defineConfig, devices } from "@playwright/test";

const e2ePort = process.env.DISPATCH_E2E_PORT || "8777";
const baseURL = process.env.PLAYWRIGHT_BASE_URL ?? `http://127.0.0.1:${e2ePort}`;
const fakeEnvoy = fileURLToPath(new URL("./fake-envoy.ts", import.meta.url));
const fakeEnvoyPort = Number(process.env.FAKE_ENVOY_PORT ?? "9021");
const fakeGithub = fileURLToPath(new URL("./fake-github.ts", import.meta.url));
const fakeGithubPort = Number(process.env.FAKE_GITHUB_PORT ?? "9022");
const runServer = fileURLToPath(new URL("./run-server.sh", import.meta.url));

// The default wait for asynchronous server and rendering readiness. A spec only sets its own
// timeout when that deadline is its observable contract (the keyboard chord-expiry test).
const expectTimeout = 15_000;

export default defineConfig({
  testDir: ".",
  testMatch: /.*\.e2e\.ts/,
  outputDir: "test-results",
  fullyParallel: false,
  workers: 1,
  reporter: "list",
  expect: { timeout: expectTimeout },
  use: {
    ...devices["Desktop Chrome"],
    baseURL,
    headless: true,
    trace: "retain-on-failure",
  },
  ...(process.env.PLAYWRIGHT_BASE_URL
    ? {}
    : {
        webServer: [
          {
            command: `bun ${fakeEnvoy}`,
            port: fakeEnvoyPort,
            reuseExistingServer: !process.env.CI,
          },
          {
            command: `bun ${fakeGithub}`,
            port: fakeGithubPort,
            reuseExistingServer: !process.env.CI,
          },
          {
            command: `bash ${runServer}`,
            port: Number(e2ePort),
            reuseExistingServer: !process.env.CI,
          },
        ],
      }),
  projects: [
    { name: "chromium", use: { ...devices["Desktop Chrome"] } },
    { name: "iphone", use: { ...devices["iPhone 13"], browserName: "chromium" } },
  ],
});
