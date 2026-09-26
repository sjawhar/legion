// @ts-nocheck — verbatim proof-sdk source. The fork emits this tree's declarations with
// `noCheck` (its tsconfig.lib.json), so it has never type-checked; see AGENTS.md.
import { test } from './harness.js';
import { history, undo } from '@milkdown/kit/prose/history';
import { EditorState, TextSelection } from '@milkdown/kit/prose/state';

import { createHeadlessProof } from '../lib-headless.js';
import { createTypedBlockCommands } from '../lib.js';
import { blockIdOf, createBlockIdsPlugin, setBlockIdGenerator } from '../editor/schema/block-ids.js';

function assert(condition: boolean, message: string): void {
  if (!condition) throw new Error(message);
}

const blockSchema = {
  version: 1,
  types: [
    {
      name: 'ask',
      content: 'paragraph+',
      render: 'host',
      attributes: {
        urgency: { kind: 'enum', choices: ['low', 'med', 'high', 'blocking'], default: 'med' },
        multiple: { kind: 'bool', default: false },
        state: { kind: 'enum', choices: ['open', 'answered', 'resolved'], default: 'open', server: true },
        answered_by: { kind: 'actor', server: true },
        answered_at: { kind: 'timestamp', server: true },
        selected: { kind: 'string[]', server: true },
        answer: { kind: 'string', server: true },
      },
    },
  ],
} as const;

async function commandsFor(markdown: string, selection?: number) {
  const proof = await createHeadlessProof({ blockId: () => 'parsed-1', blockSchema });
  let state = EditorState.create({
    schema: proof.schema,
    doc: proof.parseMarkdown(markdown),
    plugins: [history(), createBlockIdsPlugin()],
  });
  if (selection !== undefined) state = state.apply(state.tr.setSelection(TextSelection.create(state.doc, selection)));
  return {
    commands: createTypedBlockCommands({
      blockSchema,
      getState: () => state,
      dispatch: (tr) => {
        state = state.apply(tr);
      },
    }),
    state: () => state,
    apply: (tr: Parameters<typeof state.apply>[0]) => {
      state = state.apply(tr);
    },
  };
}

console.log('\n=== Typed block commands ===');

await test('insertTypedBlock creates an empty typed block with schema defaults', async () => {
  let id = 0;
  setBlockIdGenerator(() => `local-${++id}`);
  try {
    const { commands, state } = await commandsFor('Before', 1);
    assert(commands.insertTypedBlock('ask'), 'insertTypedBlock did not dispatch');
    const doc = state().doc;
    const ask = doc.child(0);
    assert(ask.type.name === 'ask', `inserted node type = ${ask.type.name}`);
    assert(ask.childCount === 1 && ask.child(0).type.name === 'paragraph', 'ask does not contain an empty paragraph');
    assert(ask.child(0).content.size === 0, 'ask body paragraph is not empty');
    assert(ask.attrs.urgency === 'med', `urgency = ${ask.attrs.urgency}`);
    assert(ask.attrs.multiple === false, `multiple = ${ask.attrs.multiple}`);
    assert(ask.attrs.state === 'open', `state = ${ask.attrs.state}`);
    assert(ask.attrs.answered_by === undefined, `answered_by = ${ask.attrs.answered_by}`);
    assert(blockIdOf(ask) === 'local-1', `ask blockId = ${blockIdOf(ask)}`);
    assert(blockIdOf(ask.child(0)) === 'local-2', `body blockId = ${blockIdOf(ask.child(0))}`);
    assert(doc.child(1).textContent === 'Before', `existing content = ${doc.child(1).textContent}`);
  } finally {
    setBlockIdGenerator(null);
  }
});

await test('retypeBlock keeps the block id and moves a paragraph into the typed body', async () => {
  setBlockIdGenerator(() => 'local-1');
  try {
    const { commands, state } = await commandsFor('Which transport?');
    const blockId = blockIdOf(state().doc.child(0));
    assert(blockId === 'parsed-1', `source blockId = ${blockId}`);
    assert(commands.retypeBlock(blockId!, 'ask', { urgency: 'high' }), 'retypeBlock did not dispatch');
    const ask = state().doc.child(0);
    assert(ask.type.name === 'ask', `retyped node type = ${ask.type.name}`);
    assert(blockIdOf(ask) === blockId, `retyped blockId = ${blockIdOf(ask)}`);
    assert(ask.child(0).textContent === 'Which transport?', `typed body = ${ask.child(0).textContent}`);
    assert(blockIdOf(ask.child(0)) === 'local-1', `body blockId = ${blockIdOf(ask.child(0))}`);
    assert(ask.attrs.urgency === 'high', `urgency = ${ask.attrs.urgency}`);
  } finally {
    setBlockIdGenerator(null);
  }
});

await test('undoing a retypeBlock restores the original plain block and its id', async () => {
  const { commands, state, apply } = await commandsFor('Which transport?');
  const blockId = blockIdOf(state().doc.child(0));
  assert(commands.retypeBlock(blockId!, 'ask'), 'retypeBlock did not dispatch');
  assert(undo(state(), apply), 'undo did not dispatch');
  const restored = state().doc.child(0);
  assert(restored.type.name === 'paragraph', `restored node type = ${restored.type.name}`);
  assert(blockIdOf(restored) === blockId, `restored blockId = ${blockIdOf(restored)}`);
  assert(restored.textContent === 'Which transport?', `restored text = ${restored.textContent}`);
});

await test('setBlockAttributes updates client-owned attributes and rejects server-owned ones', async () => {
  const { commands, state } = await commandsFor('Which transport?');
  const blockId = blockIdOf(state().doc.child(0));
  assert(commands.retypeBlock(blockId!, 'ask'), 'retypeBlock did not dispatch');
  assert(commands.setBlockAttributes(blockId!, { urgency: 'blocking', multiple: true }), 'setBlockAttributes did not dispatch');
  const ask = state().doc.child(0);
  assert(ask.attrs.urgency === 'blocking', `urgency = ${ask.attrs.urgency}`);
  assert(ask.attrs.multiple === true, `multiple = ${ask.attrs.multiple}`);
  let thrown: unknown;
  try {
    commands.setBlockAttributes(blockId!, { state: 'answered' });
  } catch (error) {
    thrown = error;
  }
  assert(thrown instanceof Error && thrown.message.includes('server-owned'), `server attr error = ${(thrown as Error | undefined)?.message}`);
});

await test('typed block menu entries require a block schema and offer paragraph retyping', async () => {
  const noSchema = createTypedBlockCommands({
    getState: () => {
      throw new Error('menu must not inspect state without a schema');
    },
    dispatch: () => {},
  });
  assert(noSchema.blockMenuItems().length === 0, 'menu entries exist without a block schema');

  const { commands } = await commandsFor('Which transport?', 1);
  assert(
    JSON.stringify(commands.blockMenuItems().map(({ label }) => label)) === JSON.stringify(['Insert Ask', 'Turn into Ask']),
    `menu labels = ${JSON.stringify(commands.blockMenuItems().map(({ label }) => label))}`,
  );
});

