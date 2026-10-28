// entity-names.ts - the character reference names the browser editor's engine decodes, for the
// pmdoc test that every one reads in Go as it does in the engine.
//
//   bun entity-names.ts <out.txt>
//
// Writes one name per line, without its `&` and `;`: the keys of the `character-entities` package
// the engine itself loads, found as the engine finds it - from the editor package through
// remark-parse, mdast-util-from-markdown, micromark and decode-named-character-reference, each
// resolved from the package before it - so a test reads the names of the copy the engine decodes
// with, not whichever copy this directory would resolve. The same install writes the same file.
import { writeFileSync } from "node:fs";
import { createRequire } from "node:module";

const [output] = process.argv.slice(2);
if (!output) {
  console.error("usage: bun entity-names.ts <out.txt>");
  process.exit(2);
}

let from = createRequire(import.meta.path).resolve("@legion/proof-editor");
for (const name of [
  "remark-parse",
  "mdast-util-from-markdown",
  "micromark",
  "decode-named-character-reference",
  "character-entities",
]) {
  from = createRequire(from).resolve(name);
}
// The path is the one resolved above, known only at run time, so a static import cannot name it.
const { characterEntities } = (await import(from)) as {
  characterEntities: Record<string, string>;
};
writeFileSync(output, `${Object.keys(characterEntities).join("\n")}\n`);
