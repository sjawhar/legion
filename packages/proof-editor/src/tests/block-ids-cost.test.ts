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
 * What one single-character edit of a 10,000-block document makes the plugin read.
 *
 * The head reads the changed spans and their ancestors only. The measured 5: the edited
 * paragraph in the old document, where the insertion point is a point and reaches no text
 * node, and the paragraph and its text node in the new one, as the plugin state lowers and
 * raises its counts; then that new pair again, as the pass looks there for a block to stamp.
 * The whole document is read once, at the load, and never again. The base read it on every
 * edit instead - 20,000 nodes here, every block and its text, the measured number this line
 * held before - so the measured cost of this edit no longer follows the document's size.
 *
 * A bound, where the base asserted equality: the base read a fixed whole document, so its
 * count was the document's. The head's count follows the shape of the edit - which spans a
 * step's map reports, and which ancestors they reach - so what the criterion holds the
 * plugin to is "at most the nodes of the changed spans and their ancestors". The measured
 * number travels in the failure message, so a regression says how far it went over.
 */
const MOST_VISITS = 5;

console.log('\n=== Block ids cost ===');

await test('one character into a 10,000-block document reads only what it changed', async () => {
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
    assert(read <= MOST_VISITS, `nodes read = ${read}, expected at most ${MOST_VISITS}`);
  } finally {
    setBlockIdVisitCounter(null);
    setBlockIdGenerator(null);
  }
});
