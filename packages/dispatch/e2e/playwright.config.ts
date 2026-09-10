import { fileURLToPath } from "node:url";
import { defineConfig, devices } from "@playwright/test";

const e2ePort = process.env.DISPATCH_E2E_PORT || "8777";
const baseURL = process.env.PLAYWRIGHT_BASE_URL ?? `http://127.0.0.1:${e2ePort}`;
const fakeEnvoy = fileURLToPath(new URL("./fake-envoy.ts", import.meta.url));
const fakeEnvoyPort = Number(process.env.FAKE_ENVOY_PORT ?? "9021");
const runServer = fileURLToPath(new URL("./run-server.sh", import.meta.url));

export default defineConfig({
  testDir: ".",
  testMatch: /.*\.e2e\.ts/,
  outputDir: "test-results",
  fullyParallel: false,
  workers: 1,
  reporter: "list",
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
