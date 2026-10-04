import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";

function json(relative: string): unknown {
  return JSON.parse(readFileSync(path.resolve(import.meta.dir, relative), "utf8"));
}

// The daemon's boot gate (`packages/daemon/internal/daemon/bootgate.go`) holds the manifest's
// `legion.daemonApiVersion` to its `DaemonAPIVersion`, which its golden test writes to this
// fixture: a bump on either side alone fails here or there, never at a boot. The manifest carries
// that one number and nothing else under `legion`.
test("package.json declares the daemon API contract version the daemon requires", () => {
  const { daemonApiVersion } = json("../../../contracts/fixtures/daemon-api/version.json") as {
    readonly daemonApiVersion: number;
  };
  const manifest = json("../../package.json") as { readonly legion: unknown };
  expect(manifest.legion).toEqual({ daemonApiVersion });
});
