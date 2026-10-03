// docs/site/media/broker/walkthrough/narrate.ts
//
// Generates narration/<id>.mp3 for the narration parts narration.json names, in the voice every
// docs video is narrated in (`NARRATION_VOICE`, docs/site/media/narration.ts):
//
//   ELEVENLABS_API_KEY=<key> bun docs/site/media/broker/walkthrough/narrate.ts [ID...]
//
// Regenerates the parts named, or every part when none is. Each part is sent with the text of the
// two parts before it and the two after it, so the voice carries from one to the next.
import { mkdirSync } from "node:fs";
import { join } from "node:path";

import { narrate } from "../../narration";
import narration from "./narration.json";

const here = import.meta.dir;
/** narration.json, in speaking order; edl.py loads it too, for the captions build.py writes. */
const parts: Record<string, string> = narration;
const ids = Object.keys(parts);
const wanted = process.argv.slice(2);
const unknown = wanted.filter((id) => !(id in parts));
if (unknown.length > 0) {
  throw new Error(`narration.json names no narration part ${unknown.join(", ")}.`);
}

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
