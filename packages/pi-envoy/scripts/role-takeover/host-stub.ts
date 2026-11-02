#!/usr/bin/env bun
/**
 * host-stub.ts — the Oh My Pi host package, for `scripts/smoke-role-takeover.sh` only.
 *
 * `extensions/envoy.ts` imports `@oh-my-pi/pi-coding-agent`, which the host supplies at run time
 * and this workspace does not install; the package's own suite replaces it with
 * `@legion/pi-shared/test/host-registry` plus a clipboard, and `omp-client.ts` runs the real
 * extension against the same stand-in. A Bun runtime plugin is how a process that is not
 * `bun test` does that, so this module is loaded with `bun --preload` ahead of the client, before
 * the extension's own import of the package is resolved.
 */
import { hostAgentRegistryMock } from "@legion/pi-shared/test/host-registry";
import { plugin } from "bun";

plugin({
  name: "oh-my-pi-host-stand-in",
  setup(build) {
    build.module("@oh-my-pi/pi-coding-agent", () => ({
      exports: {
        copyToClipboard: async (): Promise<void> => undefined,
        ...hostAgentRegistryMock,
      },
      loader: "object",
    }));
  },
});
