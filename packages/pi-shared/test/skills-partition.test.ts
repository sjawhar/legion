import { afterAll, expect, test } from "bun:test";
import { readdirSync, readFileSync, rmSync, statSync } from "node:fs";
import * as path from "node:path";
import { REPO_ROOT, stageSkills } from "./skills-guard";

// scripts/pi-plugin-prepack.sh holds the two lists that say which of the repository's skills each
// plugin ships. They must partition skills/ exactly: a directory in neither list ships with no
// plugin, so a prompt that loads it fails the daemon's boot gate; one in both ships twice, and Oh My
// Pi would discover two skills of one name. The lists are read the way the pack reads them, by
// staging each partition, rather than by parsing the script.
const skillsRoot = path.join(REPO_ROOT, "skills");
const skillDirectories = readdirSync(skillsRoot)
  .filter((entry) => statSync(path.join(skillsRoot, entry)).isDirectory())
  .sort();
const envoy = stageSkills("@sjawhar/pi-envoy");
const legion = stageSkills("@sjawhar/pi-legion");
const envoySkills = readdirSync(envoy).sort();
const legionSkills = readdirSync(legion).sort();

afterAll(() => {
  for (const staged of [envoy, legion]) {
    rmSync(path.dirname(staged), { recursive: true, force: true });
  }
});

test("the two partitions together are every skill directory", () => {
  expect([...envoySkills, ...legionSkills].sort()).toEqual(skillDirectories);
});

test("no skill is in both partitions", () => {
  expect(envoySkills.filter((skill) => legionSkills.includes(skill))).toEqual([]);
});

test("a staged skill is a copy of the repository's", () => {
  for (const [staged, skills] of [
    [envoy, envoySkills],
    [legion, legionSkills],
  ] as const) {
    for (const skill of skills) {
      expect(readdirSync(path.join(staged, skill), { recursive: true }).sort()).toEqual(
        readdirSync(path.join(skillsRoot, skill), { recursive: true }).sort()
      );
    }
  }
});

test("a package this repository does not ship has no partition", () => {
  expect(() => stageSkills("@sjawhar/pi-shared")).toThrow("unknown plugin package");
});

// Every `skill://<name>` a shipped skill or a daemon prompt names, bare or with a path, must be a
// skill one of the two plugins ships. A bare name escapes the link guard (skills-guard.ts checks
// only `skill://<name>/<path>`), so a skill could tell its reader to load one nothing ships. Code
// fences are examples and are skipped.
test("every skill:// name a shipped skill or a daemon prompt uses is a shipped skill", () => {
  const shipped = new Set([...envoySkills, ...legionSkills]);
  const roots = [envoy, legion, path.join(REPO_ROOT, "packages/daemon/internal/prompts")];
  const missing = roots.flatMap((root) =>
    readdirSync(root, { recursive: true })
      .map(String)
      .filter((file) => file.endsWith(".md"))
      .flatMap((file) => {
        const text = readFileSync(path.join(root, file), "utf8").replace(/```[\s\S]*?```/g, "");
        return [...text.matchAll(/skill:\/\/([a-z0-9](?:[a-z0-9._-]*[a-z0-9])?)/g)]
          .map(([, name]) => name)
          .filter((name) => !shipped.has(name))
          .map((name) => `${file}: skill://${name}`);
      })
  );
  expect(missing).toEqual([]);
});

// .github/workflows/release.yaml names each plugin's skills twice more, in its version arguments in
// the workflow-level `env:` (PI_ENVOY_VERSION_ARGS, PI_LEGION_VERSION_ARGS): as
// `--patch-path skills/<name>/` and as the positional `skills/<name>/` after `--`. The plugin's
// build job and legion_build pass them to `release-push.sh version`, so they decide whether a commit
// releases the plugin and which plugin pair legion_build links. A list there that is not the
// prepack's partition releases a plugin for a skill it does not ship, or misses a change to one it
// does; and release-bump.sh reads a --patch-path only when it is also a path after `--`, so both
// lists must be the partition.
const workflow: unknown = Bun.YAML.parse(
  readFileSync(path.join(REPO_ROOT, ".github/workflows/release.yaml"), "utf8")
);
const releaseEnv = new Map<string, unknown>(
  typeof workflow === "object" &&
    workflow !== null &&
    "env" in workflow &&
    typeof workflow.env === "object" &&
    workflow.env !== null
    ? Object.entries(workflow.env)
    : []
);

/** The `--patch-path` and the positional `skills/<name>/` entries of the env argument list `key`. */
function versionArgsSkills(key: string): { patchPaths: string[]; positional: string[] } {
  const args = releaseEnv.get(key);
  if (typeof args !== "string") throw new Error(`release.yaml's env has no ${key}`);
  const tokens = args.trim().split(/\s+/u);
  const separator = tokens.indexOf("--");
  if (separator === -1) throw new Error(`release.yaml's ${key} has no -- before its paths`);
  const options = tokens.slice(0, separator).join(" ");
  const patchPaths = [...options.matchAll(/--patch-path skills\/([^/\s]+)\/(?!\S)/gu)].map(
    (match) => match[1]
  );
  const positional = tokens
    .slice(separator + 1)
    .flatMap((token) => token.match(/^skills\/([^/]+)\/$/u)?.[1] ?? []);
  return { patchPaths: patchPaths.sort(), positional: positional.sort() };
}

for (const [plugin, key, staged] of [
  ["@sjawhar/pi-envoy", "PI_ENVOY_VERSION_ARGS", envoySkills],
  ["@sjawhar/pi-legion", "PI_LEGION_VERSION_ARGS", legionSkills],
] as const) {
  test(`release.yaml's skill lists for ${plugin} are its partition`, () => {
    const args = versionArgsSkills(key);
    expect(args.patchPaths, `${key}'s --patch-path entries`).toEqual(staged);
    expect(args.positional, `${key}'s positional paths after --`).toEqual(staged);
  });
}
