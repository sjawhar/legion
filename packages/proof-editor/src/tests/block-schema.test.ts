import { test } from './harness.js';
import { DISPATCH_BLOCK_POINTER_VALUES } from '@legion/contracts';
import { Schema } from '@milkdown/kit/prose/model';
import { EditorState } from '@milkdown/kit/prose/state';

import { typedBlockSpec, type BlockTypeSchema } from '../block-schema.js';
import { createHeadlessProof } from '../lib-headless.js';
import { createTypedBlockCommands } from '../typed-block-commands.js';

function assert(condition: boolean, message: string): asserts condition {
  if (!condition) throw new Error(message);
}

const blockSchema = {
  version: 1,
  types: [
    {
      name: 'callout',
      content: 'paragraph+',
      render: 'host',
      attributes: {
        kind: { kind: 'enum', choices: ['note', 'warning'], default: 'note' },
        title: { kind: 'string', default: '' },
      },
    },
    {
      name: 'transcript',
      content: 'block+',
      render: 'host',
      attributes: {},
    },
    {
      name: 'ask',
      content: 'paragraph+ bullet_list?',
      render: 'host',
      attributes: {},
    },
  ],
} as const;

console.log('\n=== Block schema ===');

await test('headless Proof registers a typed block and round-trips its remark-directive form', async () => {
  let id = 0;
  const proof = await createHeadlessProof({ blockId: () => `b-${++id}`, blockSchema });
  const markdown = ':::callout{#callout-1 kind="warning" title="Read this"}\nBody text.\n:::\n';
  const doc = proof.parseMarkdown(markdown);
  const callout = doc.child(0);
  assert(callout.type.name === 'callout', `parsed node type = ${callout.type.name}`);
  assert(callout.attrs.blockId === 'callout-1', `callout id = ${callout.attrs.blockId}`);
  assert(callout.attrs.kind === 'warning', `callout kind = ${callout.attrs.kind}`);
  assert(callout.attrs.title === 'Read this', `callout title = ${callout.attrs.title}`);
  assert(proof.serializeMarkdown(doc) === markdown, 'typed block markdown did not round-trip');
});

await test('a host drawing of a headless typed block keeps the block\'s copy attributes', async () => {
  const proof = await createHeadlessProof({
    blockId: () => 'callout-id',
    blockSchema,
    renderBlock: () => ['section', {}, ['header', {}, 'H'], ['div', { 'data-proof-block-content': '' }, 0]] as never,
  });
  const node = proof.parseMarkdown(':::callout{kind="warning" title="Risk"}\nCareful.\n:::').child(0);
  assert(node.type.name === 'callout', `parsed node type = ${node.type.name}`);
  assert(node.attrs.blockId === 'callout-id', `callout id = ${node.attrs.blockId}`);
  const drawn = JSON.stringify(proof.schema.nodes.callout.spec.toDOM!(node));
  const expected = JSON.stringify([
    'section',
    {
      'data-proof-block-attr-kind': 'warning',
      'data-proof-block-attr-title': 'Risk',
      'data-block-id': node.attrs.blockId,
    },
    ['header', {}, 'H'],
    ['div', { 'data-proof-block-content': '' }, 0],
  ]);
  assert(drawn === expected, `host-drawn toDOM = ${drawn}`);
});

const serverOwnedSchema = {
  version: 1,
  types: [
    {
      name: 'ask',
      content: 'paragraph+',
      render: 'host',
      attributes: {
        state: { kind: 'enum', choices: ['open', 'answered', 'resolved'], default: 'open', server: true },
        answered_by: { kind: 'actor', server: true },
        answered_at: { kind: 'timestamp', server: true },
        selected: { kind: 'string[]', server: true },
        answer: { kind: 'string', server: true },
      },
    },
  ],
} as const;

await test('headless Proof preserves explicitly supplied server-owned attributes', async () => {
  const proof = await createHeadlessProof({ blockSchema: serverOwnedSchema });
  const markdown = [
    ':::ask{#ask-1 state="answered" answered_by="actor-1" answered_at="2026-09-12T13:00:00Z" selected="[]" answer="Ship it"}',
    'Which option?',
    ':::',
    '',
  ].join('\n');
  const doc = proof.parseMarkdown(markdown);
  const ask = doc.child(0);
  assert(ask.attrs.state === 'answered', `ask state = ${ask.attrs.state}`);
  assert(ask.attrs.answered_by === 'actor-1', `answered_by = ${ask.attrs.answered_by}`);
  assert(ask.attrs.answered_at === '2026-09-12T13:00:00Z', `answered_at = ${ask.attrs.answered_at}`);
  assert(JSON.stringify(ask.attrs.selected) === '[]', `selected = ${JSON.stringify(ask.attrs.selected)}`);
  assert(ask.attrs.answer === 'Ship it', `answer = ${ask.attrs.answer}`);
  assert(proof.serializeMarkdown(doc) === markdown, 'server-owned attributes did not round-trip');
});

await test('headless Proof round-trips non-empty server-owned string arrays', async () => {
  const proof = await createHeadlessProof({ blockSchema: serverOwnedSchema });
  const paragraph = proof.schema.nodes.paragraph.create(null, proof.schema.text('Which option?'));
  const ask = proof.schema.nodes.ask.create({
    blockId: 'ask-1',
    state: 'answered',
    answered_by: 'actor-1',
    answered_at: '2026-09-12T13:00:00Z',
    selected: ['Ship'],
    answer: 'Ship it',
  }, paragraph);
  const markdown = proof.serializeMarkdown(proof.schema.nodes.doc.create(null, ask));
  const parsed = proof.parseMarkdown(markdown).child(0);
  assert(JSON.stringify(parsed.attrs.selected) === '["Ship"]', `selected = ${JSON.stringify(parsed.attrs.selected)}`);
  assert(proof.serializeMarkdown(proof.parseMarkdown(markdown)) === markdown, 'server string array did not round-trip');
});

await test('headless Proof omits unset server-owned attributes that have no schema default', async () => {
  const proof = await createHeadlessProof({ blockSchema: serverOwnedSchema });
  const markdown = [
    ':::ask{#ask-1 state="open"}',
    'Which option?',
    ':::',
    '',
  ].join('\n');
  const ask = proof.parseMarkdown(markdown).child(0);
  assert(ask.attrs.answered_by === undefined, `answered_by = ${ask.attrs.answered_by}`);
  assert(ask.attrs.answered_at === undefined, `answered_at = ${ask.attrs.answered_at}`);
  assert(ask.attrs.selected === undefined, `selected = ${JSON.stringify(ask.attrs.selected)}`);
  assert(ask.attrs.answer === undefined, `answer = ${ask.attrs.answer}`);
  assert(proof.serializeMarkdown(proof.parseMarkdown(markdown)) === markdown, 'unset server attrs did not round-trip');
});
await test('headless Proof round-trips a typed block with arbitrary block children', async () => {
  let id = 0;
  const proof = await createHeadlessProof({ blockId: () => `b-${++id}`, blockSchema });
  const markdown = [
    ':::transcript{#transcript-1}',
    'Opening paragraph.',
    '',
    '* First point',
    '* Second point',
    '',
    '```ts',
    'const answer = 42;',
    '```',
    '',
    '> Quoted context.',
    ':::',
    '',
  ].join('\n');
  const doc = proof.parseMarkdown(markdown);
  const transcript = doc.child(0);
  assert(transcript.type.name === 'transcript', `parsed node type = ${transcript.type.name}`);
  const serialized = proof.serializeMarkdown(doc);
  assert(serialized === markdown, `block+ typed block markdown did not round-trip: ${JSON.stringify(serialized)}`);
});

await test('headless Proof accepts a paragraph sequence followed by an optional bullet list', async () => {
  let id = 0;
  const proof = await createHeadlessProof({ blockId: () => `b-${++id}`, blockSchema });
  const markdown = [
    ':::ask{#ask-1}',
    'Which option?',
    '',
    '* First option',
    '* Second option',
    ':::',
    '',
  ].join('\n');
  const doc = proof.parseMarkdown(markdown);
  const ask = doc.child(0);
  assert(ask.type.name === 'ask', `parsed node type = ${ask.type.name}`);
  const serialized = proof.serializeMarkdown(doc);
  assert(serialized === markdown, `paragraph+ bullet_list? typed block markdown did not round-trip: ${JSON.stringify(serialized)}`);
});

await test('headless Proof round-trips directive-like content in fenced code blocks', async () => {
  const proof = await createHeadlessProof({ blockSchema });
  for (const markdown of [
    '```md\n::: {.callout}\n```\n',
    '```md\n:::callout{#callout-1 kind="warning" title="Read this"}\n```\n',
  ]) {
    assert(
      proof.serializeMarkdown(proof.parseMarkdown(markdown)) === markdown,
      `fenced directive-like code did not round-trip: ${JSON.stringify(markdown)}`,
    );
  }
});
await test('headless Proof accepts directive-like content in indented code blocks', async () => {
  const proof = await createHeadlessProof({ blockSchema });
  const doc = proof.parseMarkdown('    ::: {.callout}\n');
  assert(doc.child(0).type.name === 'code_block', `parsed node type = ${doc.child(0).type.name}`);
});
await test('headless Proof rejects Pandoc fenced div syntax outside code blocks', async () => {
  const proof = await createHeadlessProof({ blockSchema });
  let thrown: unknown;
  try {
    proof.parseMarkdown('::: {.callout}\nBody text.\n:::\n');
  } catch (error) {
    thrown = error;
  }
  assert(thrown instanceof Error, 'Pandoc fenced div syntax did not fail');
  assert(
    thrown instanceof Error
      && thrown.message === 'line 1: typed block directives use :::name{...}; Pandoc fenced divs and malformed directives are not supported',
    `Pandoc fenced div error = ${(thrown as Error).message}`,
  );
});
await test('headless Proof reads directive-shaped prose inside a line back as literal text', async () => {
  const proof = await createHeadlessProof({ blockSchema });
  const cases = [
    ['held since 16:25Z\n', 'held since 16:25Z'],
    ['see :note[value] here\n', 'see :note[value] here'],
    [':note[value] opens the line\n', ':note[value] opens the line'],
    ['ratio a:b, lines :176-177\n', 'ratio a:b, lines :176-177'],
  ] as const;
  for (const [markdown, text] of cases) {
    const doc = proof.parseMarkdown(markdown);
    assert(doc.childCount === 1, `${JSON.stringify(markdown)} parsed to ${doc.childCount} blocks`);
    assert(doc.child(0).type.name === 'paragraph', `${JSON.stringify(markdown)} parsed to ${doc.child(0).type.name}`);
    assert(doc.textContent === text, `${JSON.stringify(markdown)} text = ${JSON.stringify(doc.textContent)}`);
  }
  const strong = proof.parseMarkdown('held since **16:25Z**\n').child(0).child(1);
  assert(strong.text === '16:25Z', `strong text = ${JSON.stringify(strong.text)}`);
  assert(strong.marks.some((mark) => mark.type.name === 'strong'), 'inline marks survive around the colon');
});
await test('headless Proof rejects line-start leaf and text directives the way the document service does', async () => {
  const proof = await createHeadlessProof({ blockSchema });
  const cases = [
    ['::note{value}\n', 'line 1: leaf directives (::name) are not supported'],
    ['text\n::note\nmore\n', 'line 2: leaf directives (::name) are not supported'],
    [':callout{#block-1}\n', 'line 1: text directives (:name{...}) are not supported'],
    ['- ::note\n', 'line 1: leaf directives (::name) are not supported'],
  ] as const;
  for (const [markdown, message] of cases) {
    let thrown: unknown;
    try {
      proof.parseMarkdown(markdown);
    } catch (error) {
      thrown = error;
    }
    assert(thrown instanceof Error, `unsupported directive ${JSON.stringify(markdown)} did not fail`);
    assert(
      thrown instanceof Error && thrown.message === message,
      `unsupported directive ${JSON.stringify(markdown)} error = ${(thrown as Error).message}`,
    );
  }
});

await test('headless Proof rejects a content rule naming an unknown node type', async () => {
  let thrown: unknown;
  try {
    await createHeadlessProof({
      blockSchema: {
        version: 1,
        types: [{
          name: 'invalid',
          content: 'unknown_block+',
          render: 'host',
          attributes: {},
        }],
      },
    });
  } catch (error) {
    thrown = error;
  }
  assert(thrown instanceof Error, 'unknown content rule did not fail schema creation');
  assert(
    thrown instanceof Error && thrown.message.includes("No node type or group 'unknown_block'"),
    `unknown content rule error = ${(thrown as Error).message}`,
  );
});

const decision: BlockTypeSchema = {
  name: 'ask',
  content: 'paragraph+',
  render: 'host',
  attributes: {
    urgency: { kind: 'enum', choices: ['low', 'med', 'high'], default: 'med' },
    multiple: { kind: 'bool', default: false },
    state: { kind: 'enum', choices: ['open', 'answered'], default: 'open', server: true },
    selected: { kind: 'string[]', default: [], server: true },
  },
};

/** A schema holding the typed block `type`, its spec built with `renderBlock` when given. */
function typedSchema(type: BlockTypeSchema, renderBlock?: Parameters<typeof typedBlockSpec>[1]) {
  return new Schema({
    nodes: {
      doc: { content: 'block+' },
      paragraph: { content: 'text*', group: 'block', toDOM: () => ['p', 0] },
      text: {},
      [type.name]: typedBlockSpec(type, renderBlock),
    },
  });
}

/** The attributes a rendered DOMOutputSpec array puts on its outermost element. */
function outerAttrs(spec: unknown): Record<string, unknown> {
  assert(Array.isArray(spec) && typeof spec[1] === 'object', `toDOM = ${JSON.stringify(spec)}`);
  return (spec as [string, Record<string, unknown>])[1];
}

/** A stand-in for the pasted section element parseDOM reads. */
function section(attrs: Record<string, string>) {
  return { getAttribute: (name: string) => attrs[name] ?? null } as unknown as HTMLElement;
}

await test('a typed block carries its client-owned attributes through HTML, and never its server-owned ones', async () => {
  const host = (node: { type: { name: string } }) => ['section', { 'data-proof-block-type': node.type.name, class: 'host' }, ['header', {}, 'chrome'], ['div', {}, 0]] as never;
  for (const renderBlock of [undefined, host]) {
    const schema = typedSchema(decision, renderBlock);
    const node = schema.nodes.ask.create(
      { urgency: 'high', multiple: true, state: 'answered', selected: ['A'], blockId: 'decision' },
      schema.nodes.paragraph.create(null, schema.text('Which one?')),
    );
    const attrs = outerAttrs(schema.nodes.ask.spec.toDOM!(node));
    const label = renderBlock ? 'host renderer' : 'default renderer';
    assert(attrs['data-proof-block-attr-urgency'] === 'high', `${label}: urgency DOM attr = ${JSON.stringify(attrs)}`);
    assert(attrs['data-proof-block-attr-multiple'] === 'true', `${label}: multiple DOM attr = ${JSON.stringify(attrs)}`);
    assert(!('data-proof-block-attr-state' in attrs) && !('data-proof-block-attr-selected' in attrs), `${label}: server-owned attrs reached the DOM: ${JSON.stringify(attrs)}`);
    assert(attrs['data-block-id'] === 'decision', `${label}: block id DOM attr = ${JSON.stringify(attrs)}`);
  }

  const rule = typedSchema(decision).nodes.ask.spec.parseDOM![0] as { getAttrs: (dom: HTMLElement) => Record<string, unknown> | false | null };
  const parsed = rule.getAttrs(section({
    'data-proof-block-attr-urgency': 'high',
    'data-proof-block-attr-multiple': 'true',
    'data-proof-block-attr-state': 'answered',
    'data-proof-block-attr-selected': '["A"]',
  }));
  assert(parsed !== false && parsed !== null, 'the typed block rule did not match its own section');
  assert(parsed!.urgency === 'high' && parsed!.multiple === true, `parsed client attrs = ${JSON.stringify(parsed)}`);
  assert(!('state' in parsed!) && !('selected' in parsed!), `parsed server-owned attrs from HTML: ${JSON.stringify(parsed)}`);
  const bare = rule.getAttrs(section({}));
  assert(bare !== false && bare !== null && !('urgency' in bare), `a section with no attrs parsed as ${JSON.stringify(bare)}`);
  assert(rule.getAttrs(section({ 'data-proof-block-attr-urgency': 'urgent' })) === false, 'a value the schema refuses was parsed as the typed block');
});

await test('a pasted rendered typed block reads its content hole, not its header', async () => {
  const header = { textContent: 'ask urgency med multiple false' } as unknown as HTMLElement;
  const content = { textContent: 'Which one?' } as unknown as HTMLElement;
  const rendered = {
    children: [header, content],
    querySelector: (selector: string) => selector === '[data-proof-block-content]' ? content : null,
  } as unknown as HTMLElement;
  const legacy = {
    querySelector: () => null,
  } as unknown as HTMLElement;
  const rule = typedSchema(decision).nodes.ask.spec.parseDOM![0] as {
    contentElement?: (dom: HTMLElement) => HTMLElement;
  };
  assert(typeof rule.contentElement === 'function', 'the typed block parse rule has no contentElement');
  assert(rule.contentElement!(rendered) === content, 'the pasted typed block read its header as content');
  assert(rule.contentElement!(legacy) === legacy, 'a typed block without a content hole did not read its section');
});

const pointerSchema = {
  version: 1,
  types: [
    { name: 'requirement', content: 'paragraph+', render: 'host', attributes: {} },
    {
      name: 'claim',
      content: 'paragraph+',
      render: 'host',
      attributes: { serves: { kind: 'block[]', targets: ['requirement'] } },
    },
    blockSchema.types[0],
  ],
} as const;

/** The error `run` throws, or undefined when it returns. */
function thrownBy(run: () => unknown): Error | undefined {
  try {
    run();
  } catch (error) {
    return error as Error;
  }
  return undefined;
}

await test('a block pointer attribute reads and writes exactly the values the shared table accepts', async () => {
  const proof = await createHeadlessProof({ blockSchema: pointerSchema });
  for (const { value, ok, why } of DISPATCH_BLOCK_POINTER_VALUES) {
    const markdown = `:::claim{#c1 serves="${value}"}\nBody.\n:::\n`;
    if (ok) {
      const claim = proof.parseMarkdown(markdown).child(0);
      const want = value === '' ? [] : value.split(' ');
      assert(claim.type.name === 'claim', `${why}: parsed node type = ${claim.type.name}`);
      assert(JSON.stringify(claim.attrs.serves) === JSON.stringify(want), `${why}: serves = ${JSON.stringify(claim.attrs.serves)}`);
      // The editor writes an empty attribute as its bare name, which the server reads as `=""`
      // (pmdoc directive.go); every other value is written back exactly as it was read.
      const expected = value === '' ? markdown.replace('serves=""', 'serves') : markdown;
      const written = proof.serializeMarkdown(proof.parseMarkdown(markdown));
      assert(written === expected, `${why}: wrote ${JSON.stringify(written)}, want ${JSON.stringify(expected)}`);
      assert(proof.serializeMarkdown(proof.parseMarkdown(written)) === written, `${why}: ${JSON.stringify(written)} did not read back`);
    } else {
      const error = thrownBy(() => proof.parseMarkdown(markdown));
      assert(error !== undefined, `${why}: ${JSON.stringify(value)} was accepted`);
      assert(error!.message.includes('"serves"'), `${why}: refusal = ${error!.message}`);
    }
  }
});

await test('typed-block commands refuse a block pointer to a block of their document whose type it does not allow', async () => {
  const documentRef = 'dispatch://CORE-1/spec';
  const ref = (id: string) => `${documentRef}#b-${id}`;
  const proof = await createHeadlessProof({ blockSchema: pointerSchema });
  const { schema } = proof;
  const paragraph = (id: string | null, text: string) => schema.nodes.paragraph.create({ blockId: id }, schema.text(text));
  const doc = schema.nodes.doc.create(null, [
    schema.nodes.requirement.create({ blockId: 'r1' }, paragraph(null, 'Need.')),
    schema.nodes.callout.create({ blockId: 'k1' }, paragraph(null, 'Note.')),
    paragraph('p1', 'Prose.'),
    schema.nodes.claim.create({ blockId: 'c1' }, paragraph(null, 'Claim.')),
  ]);
  const commandsFor = (contextRef: string | undefined) => {
    let state = EditorState.create({ schema, doc });
    const commands = createTypedBlockCommands({
      blockSchema: pointerSchema,
      documentRef: contextRef,
      getState: () => state,
      dispatch: (tr) => {
        state = state.apply(tr);
      },
    });
    return { commands, state: () => state };
  };

  const allowed = commandsFor(documentRef);
  assert(allowed.commands.setBlockAttributes('c1', { serves: [ref('r1')] }), 'a pointer to a requirement was not set');
  const set = JSON.stringify(allowed.state().doc.child(3).attrs.serves);
  assert(set === JSON.stringify([ref('r1')]), `serves = ${set}`);

  for (const [target, type] of [['p1', 'paragraph'], ['k1', 'callout']]) {
    const { commands, state } = commandsFor(documentRef);
    const error = thrownBy(() => commands.setBlockAttributes('c1', { serves: [ref(target)] }));
    assert(error !== undefined, `a pointer to the ${type} ${target} was set`);
    for (const word of ['"serves"', `"${type}"`, 'requirement']) {
      assert(error!.message.includes(word), `refusal of ${target} = ${error!.message}, want it to name ${word}`);
    }
    assert(state().doc.eq(doc), `the refused pointer to ${target} changed the document`);
  }

  const elsewhere = commandsFor(documentRef);
  assert(elsewhere.commands.setBlockAttributes('c1', { serves: ['dispatch://CORE-1/artifact/notes-md#b-p1'] }), 'a pointer to another document was not set');

  const unknown = commandsFor(undefined);
  assert(unknown.commands.setBlockAttributes('c1', { serves: [ref('p1')] }), 'without documentRef, a pointer to p1 was not left to the server');

  const malformed = commandsFor(documentRef);
  const error = thrownBy(() => malformed.commands.setBlockAttributes('c1', { serves: ['dispatch://CORE-1/artifact/spec#b-r1'] }));
  assert(error !== undefined && error.message.includes('"serves"'), `a non-canonical pointer: ${error?.message}`);
  assert(malformed.state().doc.eq(doc), 'the refused non-canonical pointer changed the document');
});

await test('a retype into a type whose block pointer allows fewer targets refuses a pointer the old type held', async () => {
  const documentRef = 'dispatch://CORE-1/spec';
  const callout = `${documentRef}#b-k1`;
  // A loose block points at any block; a claim, which may hold it, points only at requirements.
  const retypeSchema = {
    version: 1,
    types: [
      pointerSchema.types[0],
      { name: 'loose', content: 'paragraph+', render: 'host', attributes: { serves: { kind: 'block[]' } } },
      { name: 'claim', content: 'block+', render: 'host', attributes: { serves: { kind: 'block[]', targets: ['requirement'] } } },
      blockSchema.types[0],
    ],
  } as const;
  const proof = await createHeadlessProof({ blockSchema: retypeSchema });
  const { schema } = proof;
  const doc = schema.nodes.doc.create(null, [
    schema.nodes.callout.create({ blockId: 'k1' }, schema.nodes.paragraph.create(null, schema.text('Note.'))),
    schema.nodes.loose.create({ blockId: 'c1', serves: [callout] }, schema.nodes.paragraph.create(null, schema.text('Claim.'))),
  ]);
  let state = EditorState.create({ schema, doc });
  const commands = createTypedBlockCommands({
    blockSchema: retypeSchema,
    documentRef,
    getState: () => state,
    dispatch: (tr) => {
      state = state.apply(tr);
    },
  });
  const error = thrownBy(() => commands.retypeBlock('c1', 'claim', { serves: [callout] }));
  assert(error !== undefined, 'a retype into a claim kept its pointer at the callout');
  for (const word of ['"serves"', '"callout"', 'requirement']) {
    assert(error!.message.includes(word), `refusal = ${error!.message}, want it to name ${word}`);
  }
  assert(state.doc.eq(doc), 'the refused retype changed the document');
});

await test('a pasted typed block carrying a refused block pointer is not read as the typed block', async () => {
  const rule = typedSchema(pointerSchema.types[1]).nodes.claim.spec.parseDOM![0] as { getAttrs: (dom: HTMLElement) => Record<string, unknown> | false | null };
  const value = 'dispatch://CORE-1/spec#b-x dispatch://CORE/artifact/handbook-md#b-y';
  const parsed = rule.getAttrs(section({ 'data-proof-block-attr-serves': value }));
  assert(parsed !== false && parsed !== null && JSON.stringify(parsed.serves) === JSON.stringify(value.split(' ')), `parsed = ${JSON.stringify(parsed)}`);
  assert(rule.getAttrs(section({ 'data-proof-block-attr-serves': 'dispatch://CORE-1/artifact/spec#b-x' })) === false, 'a refused pointer was parsed as the typed block');
});
