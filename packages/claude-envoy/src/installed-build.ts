// Which build of this plugin Claude Code is running, read off the path of the bundle it loaded.
//
// Claude Code installs a plugin under `…/plugins/cache/<marketplace>/<plugin>/<version>/`, where
// the version is the published npm version, since this plugin's marketplace entry is an `npm`
// source. Every registration names that build to the listener, so the Agents page can tell a
// session running a release from one running a checkout.
import { sep } from "node:path"

/** The segments after `…/plugins/cache/` in a plugin install path, or undefined for any other
 *  path: a source checkout, `--plugin-dir`, or a bare server config. */
function pluginCacheSegments(bundlePath: string): string[] | undefined {
  const parts = bundlePath.split(sep)
  for (let index = 0; index + 1 < parts.length; index++) {
    if (parts[index] === "plugins" && parts[index + 1] === "cache") return parts.slice(index + 2)
  }
  return undefined
}

/**
 * The version a plugin install path names (`…/plugins/cache/<marketplace>/<plugin>/<version>/…`,
 * the published npm version for this plugin, whose marketplace entry is an `npm` source), or
 * undefined for any other path.
 */
export function installedPluginVersion(bundlePath: string): string | undefined {
  return pluginCacheSegments(bundlePath)?.[2]
}
