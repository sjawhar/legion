import { test } from './harness.js';
import { createHeadlessProof } from '../lib-headless.js';

function assert(condition: unknown, message: string): asserts condition {
  if (!condition) throw new Error(message);
}

type JsonNode = { type: string; text?: string; attrs?: Record<string, unknown>; marks?: Array<{ type: string; attrs?: Record<string, unknown> }>; content?: JsonNode[] };

function inlineOf(doc: JsonNode): JsonNode[] {
  const paragraph = doc.content?.[0];
  assert(paragraph?.type === 'paragraph', `Expected a paragraph first, got ${paragraph?.type}`);
  return paragraph.content ?? [];
}

console.log('\n=== Reference-style links import ===');

// LEGION-540 round 2. A reference-style link (`[text][id]` with `[id]: url` anywhere in the
// body) is ordinary CommonMark that people and agents write; the schema has no node for the
// unresolved reference, so the parser threw, the whole body fell back to its literal source,
// and the throw left the shared engine refusing the next parses too. The reference is resolved
// to the link it names before the schema sees it.
await test('a full reference link resolves to a link mark with the definition\'s url and title', async () => {
  const { parseMarkdown } = await createHeadlessProof();
  const doc = parseMarkdown('See [the docs][d] now.\n\n[d]: https://docs.example "Docs"').toJSON();
  const inline = inlineOf(doc);
  const linked = inline.find((node) => node.marks?.some((mark) => mark.type === 'link'));
  assert(linked !== undefined, `Expected a text node with a link mark, got ${JSON.stringify(inline)}`);
  assert(linked.text === 'the docs', `Expected the link text, got ${JSON.stringify(linked.text)}`);
  const link = linked.marks?.find((mark) => mark.type === 'link');
  assert(link?.attrs?.href === 'https://docs.example', `Expected the definition's url, got ${JSON.stringify(link?.attrs)}`);
  assert(link?.attrs?.title === 'Docs', `Expected the definition's title, got ${JSON.stringify(link?.attrs)}`);
  assert(doc.content?.length === 1, `Expected the definition to leave no block behind, got ${doc.content?.length}`);
});

await test('collapsed and shortcut references resolve the same way', async () => {
  const { parseMarkdown } = await createHeadlessProof();
  for (const source of ['Read [the docs][] today.\n\n[the docs]: https://docs.example', 'Read [the docs] today.\n\n[the docs]: https://docs.example']) {
    const inline = inlineOf(parseMarkdown(source).toJSON());
    const linked = inline.find((node) => node.marks?.some((mark) => mark.type === 'link'));
    assert(linked?.text === 'the docs', `${JSON.stringify(source)}: expected a linked "the docs", got ${JSON.stringify(inline)}`);
  }
});

await test('a reference image resolves to an image node', async () => {
  const { parseMarkdown } = await createHeadlessProof();
  const inline = inlineOf(parseMarkdown('Look: ![a chart][c]\n\n[c]: https://img.example/c.png "Chart"').toJSON());
  const image = inline.find((node) => node.type === 'image');
  assert(image !== undefined, `Expected an image node, got ${JSON.stringify(inline)}`);
  assert(image.attrs?.src === 'https://img.example/c.png', `Expected the definition's url, got ${JSON.stringify(image.attrs)}`);
  assert(image.attrs?.alt === 'a chart', `Expected the alt text, got ${JSON.stringify(image.attrs)}`);
});

await test('a reference with no definition reads as the literal text its author wrote', async () => {
  const { parseMarkdown } = await createHeadlessProof();
  const inline = inlineOf(parseMarkdown('See [the docs][nowhere] now.').toJSON());
  const text = inline.map((node) => node.text ?? '').join('');
  assert(text === 'See [the docs][nowhere] now.', `Expected the literal reference, got ${JSON.stringify(text)}`);
  assert(!inline.some((node) => node.marks?.some((mark) => mark.type === 'link')), 'Expected no link mark for an undefined reference');
});

await test('the parse after a reference link still formats', async () => {
  const { parseMarkdown } = await createHeadlessProof();
  parseMarkdown('see [the docs][d] now\n\n[d]: https://docs.example');
  const inline = inlineOf(parseMarkdown('Fixed **two** bugs').toJSON());
  assert(inline.some((node) => node.text === 'two' && node.marks?.some((mark) => mark.type === 'strong')), `Expected bold "two" after the reference link, got ${JSON.stringify(inline)}`);
});
