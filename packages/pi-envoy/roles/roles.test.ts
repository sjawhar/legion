import { describe, expect, test } from "bun:test";
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";

const rolesDir = import.meta.dir;
const phaseRoles = ["planner", "implementer", "tester", "reviewer", "merger"] as const;
const cores = ["planner", "implementer", "tester", "reviewer", "oracle"] as const;
const composedWithCommon = ["planner", "implementer", "tester", "reviewer"] as const;
const modeNeutral = [
  path.join("core", "common.md"),
  ...cores.map((r) => path.join("core", `${r}.md`)),
  path.join("mechanics", "interactive.md"),
];
const headlessOnly = [
  "LEGION_",
  "legion gh",
  "legion handoff",
  "legion threads",
  "envoy_publish",
  ".legion/",
  "roleToken",
  "spawn_worker",
  "legion-worker",
];
const repoSpecific = [
  "Inspect",
  "inspect_ai",
  "inspect_",
  "Hawk",
  "middleman",
  "Taiga",
  "agent-c",
  "trajectory",
];

const read = (...parts: string[]) => readFileSync(path.join(rolesDir, ...parts), "utf8");

describe("role prompt parts", () => {
  test("cores and the interactive fragment carry no headless mechanics", () => {
    for (const file of modeNeutral) {
      const text = read(file);
      for (const needle of headlessOnly)
        expect(text, `${file} contains ${needle}`).not.toContain(needle);
    }
  });

  test("no reusable part names a repository or library", () => {
    for (const file of [...modeNeutral, path.join("mechanics", "headless.md")]) {
      const text = read(file);
      for (const needle of repoSpecific)
        expect(text, `${file} contains ${needle}`).not.toContain(needle);
    }
  });

  test("every part exists; the merger has no core", () => {
    expect(existsSync(path.join(rolesDir, "core", "common.md")), "core/common").toBe(true);
    for (const role of cores)
      expect(existsSync(path.join(rolesDir, "core", `${role}.md`)), `core/${role}`).toBe(true);
    for (const m of ["headless", "interactive"])
      expect(existsSync(path.join(rolesDir, "mechanics", `${m}.md`)), m).toBe(true);
    for (const role of phaseRoles)
      expect(existsSync(path.join(rolesDir, `${role}.md`)), `residue ${role}`).toBe(true);
    expect(existsSync(path.join(rolesDir, "core", "merger.md"))).toBe(false);
  });

  test("shared rules sit in core/common.md once; role rules sit in their cores and only there", () => {
    const readSource = "read the code that already does the nearest thing";
    const noDefer = "Nothing needed for correctness is deferred";
    const fastChecks = "run the repository's fast local checks";
    const redTest = "the test itself is not theirs to change";
    const dontModify = "make the tester's red test pass; do not modify it";
    const common = read("core", "common.md");
    expect(common).toContain(readSource);
    expect(common).toContain(noDefer);
    // The four roles that compose common.md do not repeat it; the oracle composes alone and keeps
    // the read-first rule itself, never the hardening ledger (it changes nothing).
    for (const role of composedWithCommon) {
      expect(read("core", `${role}.md`), `${role} repeats the shared opening`).not.toContain(
        readSource
      );
      expect(read("core", `${role}.md`), `${role} repeats the shared opening`).not.toContain(
        noDefer
      );
    }
    expect(read("core", "oracle.md")).toContain(readSource);
    expect(read("core", "oracle.md")).not.toContain(noDefer);
    for (const role of ["implementer", "tester"])
      expect(read("core", `${role}.md`)).toContain(fastChecks);
    expect(read("core", "tester.md")).toContain(redTest);
    expect(read("core", "implementer.md")).toContain(dontModify);
    for (const role of ["planner", "reviewer", "oracle"])
      expect(read("core", `${role}.md`)).not.toContain(dontModify);
    for (const role of ["planner", "implementer", "reviewer", "oracle"])
      expect(read("core", `${role}.md`)).not.toContain(redTest);
  });

  test("the no-new-workspace rule is stated once, in the headless fragment", () => {
    const rule = "create another workspace";
    expect(read("mechanics", "headless.md")).toContain(rule);
    for (const role of ["planner", "implementer", "tester", "reviewer"])
      expect(read(`${role}.md`), `${role} residue repeats the workspace rule`).not.toContain(rule);
  });

  test("the skills-first preamble lives in the fragments, not cores or residues", () => {
    const preamble = "find this repository's skills";
    expect(read("mechanics", "headless.md")).toContain(preamble);
    expect(read("mechanics", "interactive.md")).not.toContain(preamble); // interactive states its own one-line version
    for (const role of cores) expect(read("core", `${role}.md`)).not.toContain(preamble);
    for (const role of phaseRoles) expect(read(`${role}.md`)).not.toContain(preamble);
  });

  test("residues do not repeat shared headless mechanics", () => {
    const mechanics = read("mechanics", "headless.md");
    const sharedMechanics = [
      "Read and follow the `legion-worker` skill before acting.",
      "legion handoff complete --summary",
      "When your phase is done, stay in this session afterwards:",
      "roleToken",
    ];
    for (const role of phaseRoles) {
      const residue = read(`${role}.md`);
      for (const rule of sharedMechanics) {
        expect(mechanics).toContain(rule);
        expect(residue, `${role} residue repeats ${rule}`).not.toContain(rule);
      }
    }
  });

  test("every part starts with a heading and ends with one newline", () => {
    const parts = [
      ...modeNeutral,
      path.join("mechanics", "headless.md"),
      ...phaseRoles.map((r) => `${r}.md`),
    ];
    for (const file of parts) {
      const text = read(file);
      expect(text.startsWith("#"), `${file} starts with a heading`).toBe(true);
      expect(text.endsWith("\n") && !text.endsWith("\n\n"), `${file} ends with one newline`).toBe(
        true
      );
    }
  });
});
