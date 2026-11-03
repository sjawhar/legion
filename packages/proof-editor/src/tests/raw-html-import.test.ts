import { test } from './harness.js';
import { createHeadlessProof } from '../lib-headless.js';
import type { Node as ProseMirrorNode } from '@milkdown/prose/model';

function assert(condition: boolean, message: string): void {
  if (!condition) throw new Error(message);
}

/** Every `html` node's value, in document order: the literal characters a body draws where its
 *  author wrote raw HTML. */
function rawHtmlOf(doc: ProseMirrorNode): string[] {
  const values: string[] = [];
  doc.descendants((node) => {
    if (node.type.name === 'html') values.push(String(node.attrs.value ?? ''));
    return true;
  });
  return values;
}

/** The blocks of `doc` as the text each reads, raw HTML included: what a reader sees. */
function blocksOf(doc: ProseMirrorNode): string[] {
  const blocks: string[] = [];
  doc.forEach((block) => {
    blocks.push(block.textContent + rawHtmlOf(block).join(''));
  });
  return blocks;
}

/**
 * Five bodies of the kind the dashboard showed raw before this policy: an agent's reply to an
 * ask, formatted Markdown ending in the `<!-- legion: … -->` footer its session appends. Each is
 * written here rather than copied from production, which this public repository would then hold.
 */
const ASK_REPLIES = [
  'Taking **option B**: the migration runs in one transaction, so a failed step leaves no half-written table.\n\n<!-- legion: {"session":"01HQ8F3K2M4N5P6Q7R8S9T0V","phase":"implementer"} -->',
  'The deploy is held: `migrate --check` reports two pending revisions on staging.\n\n- `0042_add_session_index`\n- `0043_backfill_actor`\n\nI run them after the review.\n\n<!-- legion: {"session":"01HQ8F3K2M4N5P6Q7R8S9T0W","phase":"tester"} -->',
  '## Review\n\nApproved. The retry budget now covers the 503 the load balancer returns during a rolling deploy.\n\n<!-- legion: {"session":"01HQ8F3K2M4N5P6Q7R8S9T0X","phase":"reviewer"} -->',
  'Neither option, and here is why: the cache key leaves out the policy, so a body parsed one way is served to a caller that asked for the other. I add the policy to the key instead.\n\n<!-- legion: {"session":"01HQ8F3K2M4N5P6Q7R8S9T0Y","phase":"planner"} -->',
  'Merged as `a1b2c3d`. The production check is `dispatch read --issue CORE-1` on the dashboard, which now shows the comment **formatted** rather than as its own source.\n\n<!-- legion: {"session":"01HQ8F3K2M4N5P6Q7R8S9T0Z","phase":"merger"} -->',
];

console.log('\n=== Raw HTML Import Policy ===');

await test("with rawHtml 'text', a block HTML comment draws as nothing and the body still formats", async () => {
  const { parseMarkdown } = await createHeadlessProof();
  const doc = parseMarkdown(
    '**Bold** text\n\n<!-- legion: {"session":"s1","phase":"retro"} -->',
    'space',
    'text',
  );
  assert(blocksOf(doc).join('|') === 'Bold text', `Expected the comment to draw as nothing, got ${JSON.stringify(blocksOf(doc))}`);
  assert(rawHtmlOf(doc).length === 0, `Expected no raw HTML node, got ${JSON.stringify(rawHtmlOf(doc))}`);
  const strong = doc.child(0).child(0);
  assert(strong.marks.some((mark) => mark.type.name === 'strong'), 'Expected the bold run to keep its mark');
});

await test("with rawHtml 'text', a block of other raw HTML draws as its literal text in its place", async () => {
  const { parseMarkdown } = await createHeadlessProof();
  const doc = parseMarkdown(
    '**Bold** text\n\n<details>\n<summary>Logs</summary>\n</details>\n\nAfter **that**.',
    'space',
    'text',
  );
  assert(
    blocksOf(doc).join('|') === 'Bold text|<details>\n<summary>Logs</summary>\n</details>|After that.',
    `Expected the raw block kept as literal text between the two paragraphs, got ${JSON.stringify(blocksOf(doc))}`,
  );
});

await test("with rawHtml 'text', an inline HTML comment draws as nothing and inline raw HTML as its own characters", async () => {
  const { parseMarkdown } = await createHeadlessProof();
  const commented = parseMarkdown('**Bold** with an <!-- inline --> comment.', 'space', 'text');
  assert(rawHtmlOf(commented).length === 0, `Expected the inline comment dropped, got ${JSON.stringify(rawHtmlOf(commented))}`);
  assert(
    commented.textContent === 'Bold with an  comment.',
    `Expected the words around the comment kept, got ${JSON.stringify(commented.textContent)}`,
  );
  const tagged = parseMarkdown('before <img src=x> after', 'space', 'text');
  assert(
    rawHtmlOf(tagged).join('') === '<img src=x>',
    `Expected the tag kept as literal text, got ${JSON.stringify(rawHtmlOf(tagged))}`,
  );
});

await test("with rawHtml 'text', a body that is one raw-HTML line draws that line as its text", async () => {
  const { parseMarkdown } = await createHeadlessProof();
  const doc = parseMarkdown('<img src=x onerror=alert(1)>', 'space', 'text');
  assert(doc.childCount === 1 && doc.child(0).type.name === 'paragraph', `Expected one paragraph, got ${JSON.stringify(doc.toJSON())}`);
  assert(
    blocksOf(doc).join('|') === '<img src=x onerror=alert(1)>',
    `Expected the line as literal text, got ${JSON.stringify(blocksOf(doc))}`,
  );
});

await test('a parse with no policy still refuses raw HTML, so a document keeps it as its author wrote it', async () => {
  const { parseMarkdown } = await createHeadlessProof();
  let refused = false;
  try {
    parseMarkdown('**Bold** text\n\n<!-- legion: {"session":"s1"} -->');
  } catch {
    refused = true;
  }
  assert(refused, 'Expected the default policy to throw for a block HTML comment');
});

await test('each policy keeps its own parser: one policy\'s parse never serves the other', async () => {
  const { parseMarkdown } = await createHeadlessProof();
  const source = 'Text\n\n<!-- legion: {"session":"s1"} -->';
  assert(parseMarkdown(source, 'space', 'text').textContent === 'Text', 'Expected the policy to drop the comment');
  let refused = false;
  try {
    parseMarkdown(source);
  } catch {
    refused = true;
  }
  assert(refused, "Expected the default policy to refuse the source the 'text' policy had just read");
  assert(
    parseMarkdown(source, 'space', 'text').textContent === 'Text',
    "Expected the 'text' policy to read it again after the refusal",
  );
});

await test("with rawHtml 'text', each ask reply that showed raw now formats with no comment in it", async () => {
  const { parseMarkdown } = await createHeadlessProof();
  for (const reply of ASK_REPLIES) {
    const where = JSON.stringify(reply.slice(0, 40));
    let doc: ProseMirrorNode;
    try {
      doc = parseMarkdown(reply, 'space', 'text');
    } catch (error) {
      throw new Error(`Expected ${where} to parse, it threw: ${String(error)}`);
    }
    assert(rawHtmlOf(doc).length === 0, `Expected no raw HTML left in ${where}, got ${JSON.stringify(rawHtmlOf(doc))}`);
    assert(!doc.textContent.includes('legion:'), `Expected the footer gone from ${where}, got ${JSON.stringify(doc.textContent)}`);
    assert(doc.textContent.length > 0, `Expected ${where} to keep its words`);
  }
});
