import { expect, test } from "bun:test"
import { installedPluginVersion } from "../src/installed-build"

test("the installed build is the version of the plugin cache path", () => {
  const installed =
    "/home/me/.claude/plugins/cache/legion-plugins/claude-envoy/afe10b2f3e1b/dist/envoy-channel.js"

  expect(installedPluginVersion(installed)).toBe("afe10b2f3e1b")
})

test("a bundle outside the plugin cache names no installed build", () => {
  const checkout = "/work/legion/packages/claude-envoy/dist/envoy-channel.js"

  expect(installedPluginVersion(checkout)).toBeUndefined()
})
