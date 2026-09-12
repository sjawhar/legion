import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { LEGION_DAEMON_API_VERSION } from "@legion/contracts";

// The daemon refuses to start unless the installed plugin's manifest carries the daemon API
// contract version it was built against (`verifyLegionPluginContract`); this pins the two
// sides of that handshake to the same number at build time, so a contract bump that forgets
// the manifest fails here instead of on every daemon boot after the release.
test("package.json declares the daemon API contract version this plugin was built against", () => {
  const manifest: unknown = JSON.parse(
    readFileSync(path.resolve(import.meta.dir, "../../package.json"), "utf8")
  );
  expect(manifest).toMatchObject({ legion: { daemonApiVersion: LEGION_DAEMON_API_VERSION } });
});
