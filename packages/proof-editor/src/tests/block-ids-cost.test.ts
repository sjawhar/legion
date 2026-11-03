import { test } from './harness.js';
import { EditorState, TextSelection } from '@milkdown/kit/prose/state';
import type { Node as ProseMirrorNode } from '@milkdown/kit/prose/model';

import { createHeadlessProof } from '../lib-headless.js';
import {
  BLOCK_ID_ATTR,
  blockIdOf,
  createBlockIdsPlugin,
  setBlockIdGenerator,
  setBlockIdVisitCounter,
} from '../editor/schema/block-ids.js';

function assert(condition: boolean, message: string): asserts condition {
  if (!condition) throw new Error(message);
}

/** Paragraphs: the cheapest identified block, one text child each. */
const BLOCKS = 10_000;

/**
 * What one single-character edit of a 10,000-block document makes the plugin read, today.
 *
 * The base reads the whole new document on every local edit: every paragraph and its text
 * node, 2 nodes per block. `holderAfter`'s walk of the old document is lazy and this edit
 * duplicates no id, so it does not run; an edit that does duplicate one reads the old
 * document whole as well. The commit that makes the plugin read only the ranges an edit
 * changed lowers this to the nodes of those ranges and their ancestors, so this literal
 * changes with it.
 */
const BASE_VISITS = 20_000;

console.log('\n=== Block ids cost ===');

await test('one character into a 10,000-block document reads the whole document', async () => {
  const { schema } = await createHeadlessProof();
  const paragraphs: ProseMirrorNode[] = [];
  for (let i = 1; i <= BLOCKS; i += 1) {
    paragraphs.push(schema.nodes.paragraph.create({ [BLOCK_ID_ATTR]: `b-${i}` }, schema.text(`block ${i}`)));
  }
  const doc = schema.nodes.doc.create(null, paragraphs);
  assert(doc.childCount === BLOCKS, `expected ${BLOCKS} blocks, built ${doc.childCount}`);

  // Every block is identified, so the edit below must stamp nothing: minting is a failure.
  setBlockIdGenerator(() => {
    throw new Error('the edit stamped a block; the document was meant to be fully identified');
  });
  let read: number | null = null;
  setBlockIdVisitCounter((visited) => {
    read = visited;
  });
  try {
    const base = EditorState.create({ schema, doc, plugins: [createBlockIdsPlugin()] });
    const middle = Math.floor(BLOCKS / 2);
    let pos = 0;
    for (let i = 0; i < middle; i += 1) pos += base.doc.child(i).nodeSize;
    const inside = pos + 1;
    const state = base.apply(base.tr.setSelection(TextSelection.create(base.doc, inside)));
    const started = performance.now();
    const next = state.apply(state.tr.insertText('x', inside));
    const elapsed = performance.now() - started;

    assert(next.doc.childCount === BLOCKS, `the edit changed the block count: ${next.doc.childCount}`);
    assert(next.doc.child(middle).textContent.startsWith('x'), 'the character was not inserted');
    assert(blockIdOf(next.doc.child(middle)) === `b-${middle + 1}`, 'the edited block lost its id');
    assert(read !== null, 'the plugin did not report what it read');
    console.log(`  10,000 blocks, one character inserted: ${read} nodes read in ${elapsed.toFixed(1)}ms`);
    assert(read === BASE_VISITS, `nodes read = ${read}, expected ${BASE_VISITS}`);
  } finally {
    setBlockIdVisitCounter(null);
    setBlockIdGenerator(null);
  }
});
