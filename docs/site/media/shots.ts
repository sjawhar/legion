// Takes every docs screenshot: Dispatch's set (`shots.config.ts`) and each section's own
// `<section>/shots.config.ts`, all against one harness boot.
//
//   DATABASE_URL=<a database this may truncate> bun docs/site/media/shots.ts [--only id,id] [--set name]
//
// See README.md beside this file.
import { join } from "node:path";
import { parseArgs } from "node:util";

import { withHarness } from "./harness";
import { runShotSet, type ShotSet } from "./shot-runner";

const MEDIA = import.meta.dir;

const { values } = parseArgs({
  options: { only: { type: "string" }, set: { type: "string" } },
  strict: true,
});
const only = values.only === undefined ? undefined : new Set(values.only.split(","));

const configs = [
  join(MEDIA, "shots.config.ts"),
  ...[...new Bun.Glob("*/shots.config.ts").scanSync(MEDIA)].sort().map((path) => join(MEDIA, path)),
];
const sets: ShotSet<unknown>[] = [];
for (const config of configs) {
  // The sets are found on disk, so each is imported by the path found. A config exports one set
  // or a list of them.
  const exported = (await import(config)).default as ShotSet<unknown> | ShotSet<unknown>[];
  for (const set of Array.isArray(exported) ? exported : [exported]) {
    if (values.set === undefined || values.set === set.set) sets.push(set);
  }
}
if (values.set !== undefined && sets.length === 0) {
  throw new Error(`No shot set is named ${values.set}.`);
}
if (only !== undefined) {
  const known = new Set(sets.flatMap((set) => set.shots.map((shot) => shot.id)));
  const unknown = [...only].filter((id) => !known.has(id));
  if (unknown.length > 0) throw new Error(`No shot is named ${unknown.join(", ")}.`);
}

const failures = await withHarness(async (harness) => {
  const failed: string[] = [];
  for (const set of sets) failed.push(...(await runShotSet(harness, set, only)));
  return failed;
});
if (failures.length > 0) {
  console.error(`${failures.length} shot(s) failed:\n${failures.join("\n")}`);
  process.exit(1);
}
