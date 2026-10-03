// docs/site/media/broker/walkthrough/narrate.ts
//
// Generates narration/<id>.mp3 for the narration parts edl.py names, in the voice every docs video
// is narrated in (`NARRATION_VOICE`, docs/site/media/narration.ts):
//
//   ELEVENLABS_API_KEY=<key> bun docs/site/media/broker/walkthrough/narrate.ts [ID...]
//
// Regenerates the parts named, or every part when none is. Each part is sent with the text of the
// two parts before it and the two after it, so the voice carries from one to the next.
import { mkdirSync } from "node:fs";
import { join } from "node:path";

import { narrate } from "../../narration";

const here = import.meta.dir;

/** edl.py's `NARRATION`, in speaking order: the cut holds the text, so it is read from there. */
function narrationParts(): Record<string, string> {
  const read = Bun.spawnSync(
    ["python3", "-c", "import json, edl; print(json.dumps(edl.NARRATION))"],
    { cwd: here, stderr: "pipe", stdout: "pipe" }
  );
  if (read.exitCode !== 0) {
    throw new Error(`Reading edl.py's NARRATION failed:\n${read.stderr.toString().trim()}`);
  }
  return JSON.parse(read.stdout.toString()) as Record<string, string>;
}

const parts = narrationParts();
const ids = Object.keys(parts);
const wanted = process.argv.slice(2);
const unknown = wanted.filter((id) => !(id in parts));
if (unknown.length > 0) throw new Error(`edl.py names no narration part ${unknown.join(", ")}.`);

mkdirSync(join(here, "narration"), { recursive: true });
for (const id of wanted.length > 0 ? wanted : ids) {
  const at = ids.indexOf(id);
  await narrate(parts[id], join(here, "narration", `${id}.mp3`), {
    next: ids
      .slice(at + 1, at + 3)
      .map((other) => parts[other])
      .join(" "),
    previous: ids
      .slice(Math.max(0, at - 2), at)
      .map((other) => parts[other])
      .join(" "),
  });
  console.log(`narrate.ts: wrote narration/${id}.mp3`);
}
