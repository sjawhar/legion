// The repository's own owner and name, as .github/scripts/repo-paths.sh derives them: the site
// GitHub Pages serves this build at, the base it is served under, and the links back to the
// repository. The docs build shells out to that bash script instead of deriving anything here so
// that the workflows and the site read one source, and moving the repository to another owner or
// name stays one environment value. Off a runner $GITHUB_REPOSITORY is unset and the script
// answers today's sjawhar/legion, so a local `bun run docs:build` builds today's site.
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";

export type RepoPathKey =
  | "worker_image"
  | "envoy_image"
  | "plugin_download_url"
  | "repository_url"
  | "docs_site"
  | "docs_base"
  | "docs_edit_base";

const script = fileURLToPath(new URL("../../.github/scripts/repo-paths.sh", import.meta.url));

/** Every `key=value` the script prints, for this build's $GITHUB_REPOSITORY. */
export const repoPaths = Object.fromEntries(
  execFileSync(script, { encoding: "utf8" })
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => [line.slice(0, line.indexOf("=")), line.slice(line.indexOf("=") + 1)])
) as Record<RepoPathKey, string>;
