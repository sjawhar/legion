import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { LEGION_DAEMON_API_VERSION } from "@legion/contracts";

function json(relative: string): unknown {
  return JSON.parse(readFileSync(path.resolve(import.meta.dir, relative), "utf8"));
}

// The daemon refuses to start unless the installed plugin's manifest carries the daemon API
// contract version it was built against (`verifyLegionPluginContract`); this pins the two
// sides of that handshake to the same number at build time, so a contract bump that forgets
// the manifest fails here instead of on every daemon boot after the release.
test("package.json declares the daemon API contract version this plugin was built against", () => {
  expect(json("../../package.json")).toMatchObject({
    legion: { daemonApiVersion: LEGION_DAEMON_API_VERSION },
  });
});

// The Go daemon's boot gate (`packages/daemon-go/internal/daemon/bootgate.go`) holds the
// manifest's `legion.goDaemonApiVersion` to its `GoDaemonAPIVersion`, which its golden test writes
// to this fixture: a bump on either side alone fails here or there, never at a boot.
test("package.json declares the Go daemon API contract version the Go daemon requires", () => {
  const { goDaemonApiVersion } = json("../../../contracts/fixtures/daemon-api/version.json") as {
    readonly goDaemonApiVersion: number;
  };
  expect(json("../../package.json")).toMatchObject({ legion: { goDaemonApiVersion } });
});
