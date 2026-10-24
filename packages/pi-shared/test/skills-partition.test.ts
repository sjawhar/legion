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

// .github/workflows/release.yaml names each plugin's skills twice more: in the `changes` filter
// that decides whether the plugin builds (`skills/<name>/**`), and in the version step that decides
// whether a commit releases it (`--patch-path skills/<name>/` and the positional `skills/<name>/`).
// A list there that is not the prepack's partition releases a plugin for a skill it does not ship,
// or misses a change to one it does. The workflow is read as text: these lists are plain lines, and
// a YAML parse would not see inside the version step's shell command.
const release = readFileSync(path.join(REPO_ROOT, ".github/workflows/release.yaml"), "utf8").split(
  "\n"
);

/** The `skills/<name>/**` entries of the `changes` filter `key` (`pi_envoy`, `pi_legion`). */
function filterSkills(key: string): string[] {
  const start = release.findIndex((line) => line.trim() === `${key}:`);
  if (start === -1) throw new Error(`release.yaml has no changes filter ${key}`);
  const indent = release[start].indexOf(key);
  const names: string[] = [];
  for (const line of release.slice(start + 1)) {
    const trimmed = line.trim();
    if (trimmed === "") break;
    if (!trimmed.startsWith("-") && !trimmed.startsWith("#") && line.search(/\S/u) <= indent) break;
    const match = trimmed.match(/^- '?skills\/([^/']+)\/\*\*'?$/u);
    if (match !== null) names.push(match[1]);
  }
  return names.sort();
}

/** The `--patch-path` and the positional `skills/<name>/` entries of the version step of `tag`. */
function versionStepSkills(tag: string): { patchPaths: string[]; positional: string[] } {
  const start = release.findIndex((line) =>
    line.includes(`.github/scripts/release-push.sh version ${tag} `)
  );
  if (start === -1) throw new Error(`release.yaml has no version step for ${tag}`);
  const end = release.findIndex((line, index) => index > start && line.includes("$GITHUB_OUTPUT"));
  const command = release.slice(start, end + 1).join(" ");
  const patchPaths: string[] = [];
  const positional: string[] = [];
  for (const match of command.matchAll(/(--patch-path\s+)?(?<![\w/.-])skills\/([^/\s]+)\//gu)) {
    (match[1] === undefined ? positional : patchPaths).push(match[2]);
  }
  return { patchPaths: patchPaths.sort(), positional: positional.sort() };
}

for (const [plugin, key, tag, staged] of [
  ["@sjawhar/pi-envoy", "pi_envoy", "pi-envoy-v", envoySkills],
  ["@sjawhar/pi-legion", "pi_legion", "pi-legion-v", legionSkills],
] as const) {
  test(`release.yaml's skill lists for ${plugin} are its partition`, () => {
    expect(filterSkills(key), `the ${key} changes filter`).toEqual(staged);
    const step = versionStepSkills(tag);
    expect(step.patchPaths, `the ${tag} version step's --patch-path entries`).toEqual(staged);
    expect(step.positional, `the ${tag} version step's positional paths`).toEqual(staged);
  });
}
