// gen.ts — regenerate testdata/fixtures.json from testdata/corpus/*.md using the fork's headless engine.
import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import * as Y from "yjs";
import { prosemirrorToYXmlFragment } from "y-prosemirror";
import { createHeadlessProof } from "@sjawhar/proof-editor/headless";

const here = import.meta.dir;
const corpus = join(here, "..", "testdata", "corpus");
const out = join(here, "..", "testdata", "fixtures.json");
const check = process.argv.includes("--check");

const engine = await createHeadlessProof();
const fixtures = readdirSync(corpus).filter((f) => f.endsWith(".md")).sort().map((file) => {
  const markdown = readFileSync(join(corpus, file), "utf8");
  const doc = engine.parseMarkdown(markdown);
  const ydoc = new Y.Doc();
  ydoc.clientID = 1;
  prosemirrorToYXmlFragment(doc, ydoc.getXmlFragment("prosemirror"));
  return {
    name: file.replace(/\.md$/, ""),
    markdown,
    pm_json: doc.toJSON(),
    yjs_update_v1_b64: Buffer.from(Y.encodeStateAsUpdate(ydoc)).toString("base64"),
    rendered_by_milkdown: engine.serializeMarkdown(doc),
  };
});
const next = JSON.stringify(fixtures, null, 2) + "\n";
if (check) {
  const current = readFileSync(out, "utf8");
  if (current !== next) { console.error("fixtures.json is stale: run `bun run gen`"); process.exit(1); }
  console.log(`fixtures.json up to date (${fixtures.length} fixtures)`);
} else {
  writeFileSync(out, next);
  console.log(`wrote ${fixtures.length} fixtures`);
}
