import { afterAll, expect, test } from "bun:test";
import { readdirSync, rmSync, statSync } from "node:fs";
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
