// The repository's own owner and name, as .github/scripts/repo-paths.sh derives them: the site
// GitHub Pages serves this build at, the base it is served under, and the links back to the
// repository. The docs build shells out to that bash script instead of deriving anything here so
// that the workflows and the site read one source, and moving the repository to another owner or
// name stays one environment value. Off a runner $GITHUB_REPOSITORY is unset and the script
// answers today's sjawhar/legion, so a local `bun run docs:build` builds today's site.
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";

/** Every key the script prints and this site reads, which the parse below is held to. */
const repoPathKeys = [
  "worker_image",
  "envoy_image",
  "plugin_download_url",
  "repository_url",
  "docs_site",
  "docs_base",
  "docs_edit_base",
] as const;

type RepoPathKey = (typeof repoPathKeys)[number];

const script = fileURLToPath(new URL("../../.github/scripts/repo-paths.sh", import.meta.url));

/** Each `key=value` line the script printed, for this build's $GITHUB_REPOSITORY. */
const parsed = Object.fromEntries(
  execFileSync(script, { encoding: "utf8" })
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => {
      const split = line.indexOf("=");
      return [line.slice(0, split), line.slice(split + 1)];
    })
);

// A key the script stops printing — renamed, or dropped — would otherwise leave the types green
// and the value undefined, and Astro would build with `site: undefined` rather than fail: so the
// keys this site reads are checked against what the script printed before anything reads one.
for (const key of repoPathKeys) {
  if (typeof parsed[key] !== "string") {
    throw new Error(`.github/scripts/repo-paths.sh printed no ${key}: the docs build reads it`);
  }
}

/** Every path the script derives for this build's $GITHUB_REPOSITORY. */
export const repoPaths = parsed as Record<RepoPathKey, string>;
