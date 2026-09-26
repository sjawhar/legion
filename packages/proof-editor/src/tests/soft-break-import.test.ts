// @ts-nocheck — verbatim proof-sdk source. The fork emits this tree's declarations with
// `noCheck` (its tsconfig.lib.json), so it has never type-checked; see AGENTS.md.
import { test } from './harness.js';
import { createHeadlessProof } from '../lib-headless.js';

function assert(condition: boolean, message: string): void {
  if (!condition) throw new Error(message);
}

console.log('\n=== Soft Break Import Normalization ===');

await test('a CommonMark soft break imports as a single text node with a space', async () => {
  const { parseMarkdown } = await createHeadlessProof();
  const doc = parseMarkdown('First soft line\ncontinues in the same paragraph.').toJSON();
  const paragraph = doc.content[0];
  assert(paragraph.type === 'paragraph', 'Expected a single paragraph node');
  assert(paragraph.content.length === 1, 'Expected the soft break to stay inside one text node, not split it');
  assert(
    paragraph.content[0].text === 'First soft line continues in the same paragraph.',
    `Expected the soft break normalized to a space, got ${JSON.stringify(paragraph.content[0].text)}`,
  );
});

await test('a fenced code block keeps its literal newlines', async () => {
  const { parseMarkdown } = await createHeadlessProof();
  const doc = parseMarkdown('```\nline one\nline two\n```').toJSON();
  const codeBlock = doc.content[0];
  assert(codeBlock.type === 'code_block', 'Expected a code_block node');
  assert(
    codeBlock.content[0].text === 'line one\nline two',
    `Expected code block newlines preserved, got ${JSON.stringify(codeBlock.content[0].text)}`,
  );
});

await test('a hard break (two trailing spaces) still yields a hardbreak node', async () => {
  const { parseMarkdown } = await createHeadlessProof();
  const doc = parseMarkdown('Line one  \nLine two').toJSON();
  const paragraph = doc.content[0];
  assert(paragraph.type === 'paragraph', 'Expected a single paragraph node');
  const types = paragraph.content.map((node: { type: string }) => node.type);
  assert(
    types.join(',') === 'text,hardbreak,text',
    `Expected text/hardbreak/text, got ${JSON.stringify(types)}`,
  );
  assert(paragraph.content[0].text === 'Line one', 'Expected first text node unchanged by the hard break');
  assert(paragraph.content[2].text === 'Line two', 'Expected second text node unchanged by the hard break');
});

